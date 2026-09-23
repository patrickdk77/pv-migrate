package bucketstorage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"helm.sh/helm/v4/pkg/action"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/kube"
	"k8s.io/client-go/kubernetes"

	"github.com/utkuozdemir/pv-migrate/internal/archive"
	"github.com/utkuozdemir/pv-migrate/internal/console"
	"github.com/utkuozdemir/pv-migrate/internal/helm"
	"github.com/utkuozdemir/pv-migrate/internal/k8s"
	"github.com/utkuozdemir/pv-migrate/internal/narrate"
	"github.com/utkuozdemir/pv-migrate/internal/opid"
	"github.com/utkuozdemir/pv-migrate/internal/pvc"
	"github.com/utkuozdemir/pv-migrate/internal/rclone"
)

// DefaultPrefix is the bucket prefix a backup gets when none is asked for.
// It lives here rather than in the public package so the archive workflow,
// which has no prefix, can tell a defaulted one from a chosen one.
const DefaultPrefix = "pv-migrate"

const (
	dataMountPath = "/data"
	// archiveMountPath is where the claim holding the archive file is mounted,
	// which is a second volume alongside the one being backed up or restored.
	archiveMountPath = "/archive"
	// rcloneConfigMountPath is where the chart mounts the generated rclone.conf.
	rcloneConfigMountPath = "/etc/rclone/rclone.conf"
	// Keep in sync with the pvmigrate user created in docker/rclone/Dockerfile.
	nonRootUID = 10000
)

// safeBucketSegment matches strings that are safe for use in bucket paths:
// alphanumeric, hyphens, underscores, and dots.
var safeBucketSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

var helmProviders = getter.All(cli.New())

// Request holds all parameters for a backup or restore operation.
type Request struct {
	ID                    string
	ImageTag              string
	ChartVersion          string
	Direction             string // rclone.DirectionBackup or rclone.DirectionRestore
	KubeconfigPath        string
	Context               string
	Namespace             string
	PVCName               string
	IgnoreMounted         bool
	NonRoot               bool
	Detach                bool
	NoCleanup             bool
	NoCleanupOnFailure    bool
	DeleteExtraneousFiles bool

	// Bucket storage config
	Backend               string
	Bucket                string
	S3Provider            string
	Endpoint              string
	Region                string
	AccessKey             string
	SecretKey             string
	StorageAccount        string
	StorageKey            string
	GCSServiceAccountJSON string
	GCSBucketPolicyOnly   *bool
	Name                  string
	Prefix                string
	Path                  string
	RcloneConfigFile      string
	Remote                string
	RcloneExtraArgs       string

	// ArchiveFile selects the archive workflow: the volume is written to or
	// read from a single tar file instead of being synced to a bucket. Its
	// value is "<claim>:<path>" for a file on a claim, or a bare path for one
	// on the filesystem of the process running this. Compression comes from the
	// path's extension.
	ArchiveFile      string
	CompressionLevel int

	// Snapshot config. Snapshot cuts a VolumeSnapshot of the claim and backs
	// up a clone of it. FromSnapshot names an existing one to clone instead.
	// Flush quiesces the database in the pod that has the claim mounted while
	// the snapshot is cut, and names which kind of database it is.
	Snapshot       bool
	FromSnapshot   string
	SnapshotClass  string
	KeepSnapshot   bool
	Flush          string
	FlushContainer string
	FlushCommand   []string

	// FlushUser is who the flush client logs in as, empty for what the
	// database's image seeds. The password comes from FlushPassword, or from
	// a Secret in the claim's namespace named by FlushPasswordSecret as
	// "name" or "name:key"; one of the two at most.
	FlushUser           string
	FlushPassword       string
	FlushPasswordSecret string

	HelmTimeout      time.Duration
	HelmValuesFiles  []string
	HelmValues       []string
	HelmFileValues   []string
	HelmStringValues []string

	Writer io.Writer
	Logger *slog.Logger

	// StructuredLogs reports that the logger writes machine-readable records to
	// the same stream as Writer. Plain-text blocks are suppressed then, and the
	// same information is emitted as log records instead.
	StructuredLogs bool

	// ColorOutput colors the plain-text report blocks semantically. Set only
	// when the writer is a terminal and the logs are not machine-readable.
	ColorOutput bool
}

// Run executes a backup or restore operation.
//
//nolint:cyclop,funlen
func Run(ctx context.Context, req *Request) (retErr error) {
	logger := req.Logger

	// Only the public API defaults the writer, so a direct caller can leave it
	// unset. Everything below writes to it without checking.
	if req.Writer == nil {
		req.Writer = io.Discard
	}

	operationID := req.ID
	if operationID == "" {
		operationID = opid.Generate()
	}

	var err error

	var target archive.Target

	if isArchive(req) {
		if target, err = parseArchiveTarget(req, time.Now()); err != nil {
			return err
		}
	}

	if err = validateSnapshotRequest(req); err != nil {
		return err
	}

	releaseName := opid.ReleasePrefix + operationID + "-" + req.Direction

	rcloneConf, remotePath, err := resolveTarget(req, target)
	if err != nil {
		return err
	}

	localPath := dataMountPath

	if req.Path != "" {
		if err = validateSubpath(req.Path); err != nil {
			return fmt.Errorf("invalid --path: %w", err)
		}

		localPath = path.Join(dataMountPath, req.Path)
	}

	client, err := k8s.GetClusterClient(req.KubeconfigPath, req.Context, logger)
	if err != nil {
		return fmt.Errorf("failed to get cluster client: %w", err)
	}

	ns := req.Namespace
	if ns == "" {
		ns = client.NsInContext
	}

	// A snapshot-backed run never mounts the live claim, so a mounted
	// ReadWriteOncePod claim, which New refuses, is fine as a source.
	lookup := pvc.New
	if usesSnapshot(req) {
		lookup = pvc.NewSource
	}

	pvcInfo, err := lookup(ctx, client, ns, req.PVCName)
	if err != nil {
		return fmt.Errorf("failed to get PVC info: %w", err)
	}

	// Loaded before any snapshot is cut, so a chart problem costs nothing.
	helmChart, err := helm.LoadChart(req.ChartVersion)
	if err != nil {
		return fmt.Errorf("failed to load helm chart: %w", err)
	}

	destination := remotePath

	switch {
	case target.InCluster():
		destination = target.Claim + ":" + target.Path
	case target.InBucket():
		destination = "s3://" + target.Bucket + "/" + target.Path
	}

	if req.Direction == rclone.DirectionBackup {
		logger.Info(fmt.Sprintf("📦 Backing up %s/%s to %s", ns, req.PVCName, destination))
	} else {
		logger.Info(fmt.Sprintf("📥 Restoring %s to %s/%s", destination, ns, req.PVCName))
	}

	details := narrate.Detail(logger, 1)
	details.Info(fmt.Sprintf("🆔 operation id %s, for status and cleanup", operationID))
	details.Info("📌 claim " + pvcInfo.Describe())

	pvcInfo, release, err := resolveSource(ctx, client, req, pvcInfo, releaseName, details, logger)
	if err != nil {
		return err
	}

	defer func() { release(retErr != nil) }()

	var archiveInfo *pvc.Info

	if isArchive(req) {
		if target.InCluster() {
			if archiveInfo, err = resolveArchiveClaim(ctx, client, ns, target, pvcInfo); err != nil {
				return err
			}

			details.Info("💾 archive claim " + archiveInfo.Describe())
		} else if !target.InBucket() {
			// A path with no claim in front of it is a path inside the job's own
			// container, which is worth saying outright: nothing is mounted
			// there automatically, and an unmounted path is ephemeral storage
			// that goes away with the pod, taking the backup with it.
			details.Warn("🔶 " + target.Path + " is a path inside the job pod. " +
				"Mount a volume there with a --helm-values file that lists it under rclone.pvcMounts " +
				"alongside the data mount, or the archive is written to ephemeral storage and lost")
		}
	}

	if req.DeleteExtraneousFiles {
		details.Info("❕ files missing on the source will be deleted from the destination")
	}

	cmdStr, err := buildMoverCmd(req, target, localPath, remotePath)
	if err != nil {
		return err
	}

	readOnly := req.Direction == rclone.DirectionBackup

	meta, err := buildMetadata(req, target, ns)
	if err != nil {
		return err
	}

	helmVals := buildHelmValues(ns, req, pvcInfo, archiveInfo, rcloneConf, cmdStr, readOnly, meta)

	if err = installHelmChart(ctx, helmChart, pvcInfo, releaseName, helmVals, req, logger); err != nil {
		// A timed-out install means resources that are stuck rather than absent,
		// and this path runs no cleanup, so they are still there to be read.
		writeFailure(ctx, req, client.KubeClient, ns, releaseName, err, logger)

		return fmt.Errorf("failed to install helm chart: %w", err)
	}

	jobName := releaseName + "-" + jobSuffix(req)

	return handleJobCompletion(ctx, req, pvcInfo, releaseName, jobName, operationID, logger)
}

// narrateMoverJob reports the job the release created, named and described by
// the data mover it actually runs.
func narrateMoverJob(
	ctx context.Context,
	req *Request,
	pvcInfo *pvc.Info,
	releaseName string,
	rcloneVals map[string]any,
	deeper *slog.Logger,
) {
	namespace := helm.ComponentNamespace(rcloneVals)
	jobName := releaseName + "-" + jobSuffix(req)

	deeper.Info(fmt.Sprintf(
		"🚚 %s job %s in namespace %s, image %s, %s",
		jobSuffix(req),
		jobName,
		namespace,
		k8s.JobImage(ctx, pvcInfo.ClusterClient.KubeClient, namespace, jobName),
		helm.DescribeMounts(rcloneVals),
	))
}

func buildRcloneConfig(req *Request) (string, error) {
	if req.RcloneConfigFile != "" {
		conf, err := rclone.ReadConfigFile(req.RcloneConfigFile)
		if err != nil {
			return "", fmt.Errorf("failed to read rclone config file: %w", err)
		}

		return conf, nil
	}

	opts := rclone.ConfigOptions{
		Backend:               req.Backend,
		Provider:              req.S3Provider,
		Endpoint:              req.Endpoint,
		Region:                req.Region,
		AccessKey:             req.AccessKey,
		SecretKey:             req.SecretKey,
		StorageAccount:        req.StorageAccount,
		StorageKey:            req.StorageKey,
		GCSServiceAccountJSON: req.GCSServiceAccountJSON,
		GCSBucketPolicyOnly:   req.GCSBucketPolicyOnly,
	}

	conf, err := rclone.GenerateConfig(opts)
	if err != nil {
		return "", fmt.Errorf("failed to generate rclone config: %w", err)
	}

	return conf, nil
}

func buildRemotePath(req *Request) (string, error) {
	if req.RcloneConfigFile != "" {
		if req.Remote == "" {
			return "", errors.New("--remote is required when using --rclone-config")
		}

		return rclone.BuildRemotePathRaw(req.Remote), nil
	}

	if req.Bucket == "" {
		return "", errors.New("--bucket is required")
	}

	if req.Name == "" {
		return "", errors.New("--name is required")
	}

	// The bucket is part of the same object path as the prefix and the name, so it
	// goes through the same rule rather than being the one segment nobody checks.
	if err := validateBucketSegment(req.Bucket, "bucket"); err != nil {
		return "", err
	}

	if err := ValidateName(req.Name); err != nil {
		return "", err
	}

	if err := ValidatePrefix(req.Prefix); err != nil {
		return "", err
	}

	return rclone.BuildRemotePath(req.Bucket, req.Prefix, req.Name), nil
}

func shouldUploadMetadata(req *Request) bool {
	return req.Direction == rclone.DirectionBackup &&
		req.RcloneConfigFile == "" &&
		!hasRcloneDryRun(req.RcloneExtraArgs)
}

// shortFlagCluster matches a run of rclone short flags, which are letters only.
// Requiring the whole token to be letters keeps a value like -1n from reading as
// a dry run.
var shortFlagCluster = regexp.MustCompile(`^-[a-zA-Z]+$`)

// hasRcloneDryRun reports whether the extra rclone arguments ask for a dry run,
// in which case the metadata sidecar must not be uploaded: a run that transfers
// nothing has no business writing a real object to the bucket.
func hasRcloneDryRun(extraArgs string) bool {
	// pflag applies each occurrence in order, so a later one overrides an earlier
	// one and the last is what rclone ends up with. Answering on the first match
	// instead would get "--dry-run=false -n" and "-n --dry-run=false" both wrong,
	// in opposite directions.
	dryRun := false

	for arg := range strings.FieldsSeq(extraArgs) {
		name, value, assigned := strings.Cut(arg, "=")

		if assigned {
			applyAssignedFlag(&dryRun, name, value)

			continue
		}

		// rclone uses pflag, so -n may be bundled with other short flags, as in -nv.
		if arg == "--dry-run" || (shortFlagCluster.MatchString(arg) && strings.Contains(arg, "n")) {
			dryRun = true
		}
	}

	return dryRun
}

// applyAssignedFlag applies one `name=value` argument to the dry-run state.
//
// A boolean flag's value is read with strconv.ParseBool, so "1", "T" and "TRUE"
// ask for a dry run just as much as "true" does, and "0" does not.
func applyAssignedFlag(dryRun *bool, name, value string) {
	switch {
	case name == "--dry-run":
		setFromBool(dryRun, value)
	case shortFlagCluster.MatchString(name):
		// pflag walks a bundle left to right, setting each shorthand on its own, and
		// gives the assigned value only to the last one. So -vn=false turns the dry run
		// off, while -nv=false leaves it on because there the n was already set bare.
		letters := strings.TrimPrefix(name, "-")

		for i, letter := range letters {
			switch {
			case letter != 'n':
			case i == len(letters)-1:
				setFromBool(dryRun, value)
			default:
				*dryRun = true
			}
		}
	}
}

// setFromBool leaves the target alone when the value is not a boolean at all,
// since rclone would reject such an argument itself.
func setFromBool(target *bool, value string) {
	if enabled, err := strconv.ParseBool(value); err == nil {
		*target = enabled
	}
}

func validateBucketSegment(value, flag string) error {
	if safeBucketSegment.MatchString(value) {
		return nil
	}

	return fmt.Errorf("--%s %q contains invalid characters (allowed: alphanumeric, hyphens, underscores, dots)",
		flag, value)
}

// ValidateName validates a managed backup name for use in bucket object paths.
func ValidateName(name string) error {
	return validateBucketSegment(name, "name")
}

// ValidatePrefix validates a managed backup prefix for use in bucket object paths.
func ValidatePrefix(prefix string) error {
	return validatePrefix(prefix)
}

func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}

	for segment := range strings.SplitSeq(prefix, "/") {
		if segment == "" {
			return fmt.Errorf(
				"--prefix %q is invalid: must not have leading/trailing '/' or empty path segments",
				prefix,
			)
		}

		if err := validateBucketSegment(segment, "prefix"); err != nil {
			return fmt.Errorf("--prefix %q contains invalid characters (allowed: slash-separated segments of "+
				"alphanumeric, hyphens, underscores, dots)", prefix)
		}
	}

	return nil
}

func buildHelmValues(
	namespace string,
	req *Request,
	pvcInfo, archiveInfo *pvc.Info,
	rcloneConf, cmdStr string,
	readOnly bool,
	meta metadataValues,
) map[string]any {
	mounts := []map[string]any{
		{
			"name":      pvcInfo.Claim.Name,
			"mountPath": dataMountPath,
			"readOnly":  readOnly,
		},
	}

	affinity := pvcInfo.AffinityHelmValues

	if archiveInfo != nil {
		// The archive claim is written on backup and read on restore, which is
		// the opposite of the claim holding the data.
		mounts = append(mounts, map[string]any{
			"name":      archiveInfo.Claim.Name,
			"mountPath": archiveMountPath,
			"readOnly":  !readOnly,
		})

		affinity = archiveAffinity(pvcInfo, archiveInfo)
	}

	rcloneVals := map[string]any{
		"enabled":   true,
		"namespace": namespace,
		"jobSuffix": jobSuffix(req),
		// A tar job writing to a volume has no remote and so no config; an
		// rclone job, or a tar job streaming to S3, does.
		"configMount": rcloneConf != "",
		"config":      rcloneConf,
		"command":     cmdStr,
		"extraArgs":   "",
		"pvcMounts":   mounts,
		"affinity":    affinity,
	}

	if meta.base64 != "" {
		rcloneVals["metadataBase64"] = meta.base64
		rcloneVals["metadataRemotePath"] = meta.remotePath
		rcloneVals["metadataLocalPath"] = meta.localPath
	}

	vals := map[string]any{
		"rclone": rcloneVals,
	}

	if req.NonRoot {
		applyNonRootValues(vals)
	}

	return vals
}

func applyNonRootValues(vals map[string]any) {
	rcloneSection, ok := vals["rclone"].(map[string]any)
	if !ok {
		return
	}

	rcloneSection["securityContext"] = map[string]any{
		"runAsNonRoot":             true,
		"runAsUser":                nonRootUID,
		"runAsGroup":               nonRootUID,
		"allowPrivilegeEscalation": false,
	}
	rcloneSection["podSecurityContext"] = map[string]any{
		"fsGroup": nonRootUID,
	}
}

func installHelmChart(
	ctx context.Context,
	helmChart *chart.Chart,
	pvcInfo *pvc.Info,
	releaseName string,
	baseValues map[string]any,
	req *Request,
	logger *slog.Logger,
) error {
	actionConfig := new(action.Configuration)

	err := actionConfig.Init(pvcInfo.ClusterClient.RESTClientGetter,
		pvcInfo.Claim.Namespace, os.Getenv("HELM_DRIVER"))
	if err != nil {
		return fmt.Errorf("failed to initialize helm action config: %w", err)
	}

	install := action.NewInstall(actionConfig)
	install.Namespace = pvcInfo.Claim.Namespace
	install.ReleaseName = releaseName
	install.WaitStrategy = kube.LegacyStrategy
	install.Timeout = req.HelmTimeout

	// Before the user's values are merged on top, so that an explicit request
	// for the policy is still honored, and the install then reports the real
	// permission problem.
	canCreate := func(ctx context.Context, namespace string) (bool, error) {
		return k8s.CanCreateNetworkPolicies(ctx, pvcInfo.ClusterClient.KubeClient, namespace)
	}

	helm.DisableNetworkPoliciesWhereForbidden(ctx, baseValues, canCreate, narrate.Detail(logger, 1))

	merged, err := mergeHelmValues(baseValues, req)
	if err != nil {
		return err
	}

	if _, err = install.Run(helmChart, merged); err != nil {
		return fmt.Errorf("failed to install helm chart: %w", err)
	}

	details := narrate.Detail(logger, 1)
	deeper := narrate.Detail(logger, 2)

	details.Info("📦 created release " + releaseName)

	if rcloneVals, ok := helm.EnabledComponent(merged, "rclone"); ok {
		narrateMoverJob(ctx, req, pvcInfo, releaseName, rcloneVals, deeper)

		if helm.NetworkPolicyOn(rcloneVals) {
			deeper.Info("🔒 network policy for the rclone pod, so a default-deny namespace does not block it")
		}
	}

	return nil
}

func mergeHelmValues(baseValues map[string]any, req *Request) (map[string]any, error) {
	helmValues := req.HelmValues
	if tag := req.ImageTag; tag != "" {
		helmValues = append([]string{"rclone.image.tag=" + tag}, req.HelmValues...)
	}

	valsOptions := values.Options{
		ValueFiles:   req.HelmValuesFiles,
		Values:       helmValues,
		StringValues: req.HelmStringValues,
		FileValues:   req.HelmFileValues,
	}

	userValues, err := valsOptions.MergeValues(helmProviders)
	if err != nil {
		return nil, fmt.Errorf("failed to merge helm values: %w", err)
	}

	return loader.MergeMaps(baseValues, userValues), nil
}

func handleJobCompletion(
	ctx context.Context,
	req *Request,
	pvcInfo *pvc.Info,
	releaseName, jobName, operationID string,
	logger *slog.Logger,
) (retErr error) {
	kubeClient := pvcInfo.ClusterClient.KubeClient
	namespace := pvcInfo.Claim.Namespace

	defer func() {
		if req.NoCleanup {
			narrate.Detail(logger, 1).Info("🧹 cleanup skipped, the resources stay in the cluster")

			return
		}

		if req.NoCleanupOnFailure && retErr != nil {
			narrate.Detail(logger, 1).
				Info("🧹 cleanup skipped since the operation failed, the resources stay for inspection")

			return
		}

		if req.Detach {
			return
		}

		details := narrate.Detail(logger, 1)

		if cleanupErr := cleanupRelease(pvcInfo, releaseName, req.HelmTimeout); cleanupErr != nil {
			details.Warn("🔶 cleanup failed, clean up with pv-migrate cleanup: " + cleanupErr.Error())
		} else {
			details.Info(fmt.Sprintf("🧹 removed release %s from namespace %s", releaseName, namespace))
		}
	}()

	started := time.Now()

	if req.Detach {
		if _, err := k8s.WaitForJobStart(ctx, kubeClient, namespace, jobName, narrate.Detail(logger, 1)); err != nil {
			return fmt.Errorf("failed to wait for job to start: %w", err)
		}

		printDetachMessage(req, operationID, logger)

		return nil
	}

	if err := k8s.WaitForJobCompletion(ctx, kubeClient, namespace, jobName,
		shouldShowProgressBar(req.Writer), req.StructuredLogs,
		console.Palette{Enabled: req.ColorOutput}, req.Writer, narrate.Detail(logger, 1)); err != nil {
		// Before the deferred cleanup removes the resources this is about.
		writeFailure(ctx, req, kubeClient, namespace, releaseName, err, logger)

		// Deliberately unwrapped: the error already names the failed pod and the
		// exit state, the public API adds the operation prefix, and a plumbing
		// wrap in between would push the answer further right.
		return err //nolint:wrapcheck
	}

	logger.Info(fmt.Sprintf("✅ %s succeeded in %s", capitalizedDirection(req.Direction),
		time.Since(started).Round(time.Second)))

	return nil
}

// writeFailure explains a failure the way the migration summary does: a heading,
// the cause on an indented line beneath it, then what the cluster reported. On a
// structured log stream the plain-text block would corrupt the records around
// it, so it travels inside a record instead.
func writeFailure(
	ctx context.Context,
	req *Request,
	kubeClient kubernetes.Interface,
	namespace, releaseName string,
	cause error,
	logger *slog.Logger,
) {
	if req.StructuredLogs {
		var buf bytes.Buffer

		k8s.WriteWorkloadDiagnostics(ctx, kubeClient, namespace,
			k8s.InstanceLabelSelector(releaseName), console.Palette{}, &buf, logger)

		logger.Error("❌ What the cluster reported", "release", releaseName,
			"namespace", namespace, "diagnostics", buf.String())

		return
	}

	palette := console.Palette{Enabled: req.ColorOutput}

	fmt.Fprintf(req.Writer, "\n%s\n", palette.Failure(capitalizedDirection(req.Direction)+" failed."))

	if cause != nil {
		for line := range strings.SplitSeq(cause.Error(), "\n") {
			fmt.Fprintf(req.Writer, "    %s\n", line)
		}
	}

	fmt.Fprintf(req.Writer, "\n%s\n\n  %s (namespace %s):\n",
		palette.Bold("What the cluster reported:"), releaseName, namespace)
	k8s.WriteWorkloadDiagnostics(ctx, kubeClient, namespace,
		k8s.InstanceLabelSelector(releaseName), palette, req.Writer, logger)
}

func capitalizedDirection(direction string) string {
	if direction == "" {
		return "Operation"
	}

	return strings.ToUpper(direction[:1]) + direction[1:]
}

func cleanupRelease(pvcInfo *pvc.Info, releaseName string, timeout time.Duration) error {
	actionConfig := new(action.Configuration)

	err := actionConfig.Init(pvcInfo.ClusterClient.RESTClientGetter,
		pvcInfo.Claim.Namespace, os.Getenv("HELM_DRIVER"))
	if err != nil {
		return fmt.Errorf("failed to initialize helm action config: %w", err)
	}

	uninstall := action.NewUninstall(actionConfig)
	uninstall.WaitStrategy = kube.LegacyStrategy
	uninstall.Timeout = timeout

	if _, err = uninstall.Run(releaseName); err != nil {
		return fmt.Errorf("failed to uninstall helm release %s: %w", releaseName, err)
	}

	return nil
}

func handleMounted(info *pvc.Info, ignoreMounted bool, logger *slog.Logger) error {
	if info.MountedNode == "" {
		return nil
	}

	if ignoreMounted {
		logger.Info(fmt.Sprintf("💡 %s/%s is mounted on node %s, continuing because --ignore-mounted is set",
			info.Claim.Namespace, info.Claim.Name, info.MountedNode))

		return nil
	}

	return fmt.Errorf("PVC is mounted to a node and --ignore-mounted is not requested: "+
		"node: %s, claim: %s/%s", info.MountedNode, info.Claim.Namespace, info.Claim.Name)
}

// validateSubpath ensures the path is a relative subpath that stays under the mount root.
func validateSubpath(p string) error {
	if path.IsAbs(p) {
		return errors.New("must be a relative path")
	}

	cleaned := path.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("must not escape the volume root with '..'")
	}

	return nil
}

func printDetachMessage(req *Request, operationID string, logger *slog.Logger) {
	logger.Info(fmt.Sprintf("🚀 %s detached, the rclone job keeps running in the cluster",
		capitalizedDirection(req.Direction)))

	fmt.Fprintln(req.Writer)
	fmt.Fprintf(req.Writer, "%s %s detached. The rclone job is running in the cluster.\n",
		req.Direction, operationID)
	fmt.Fprintln(req.Writer)
	fmt.Fprintln(req.Writer, "To check status:")
	fmt.Fprintf(req.Writer, "  pv-migrate status %s\n", operationID)
	fmt.Fprintln(req.Writer)
	fmt.Fprintln(req.Writer, "To clean up after completion:")
	fmt.Fprintf(req.Writer, "  pv-migrate cleanup %s\n", operationID)
}

func shouldShowProgressBar(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}

	return isatty.IsTerminal(file.Fd())
}

// jobSuffix names the data mover the job runs. It ends the job's name, which
// is how the exit-code table and the progress parser are chosen for it.
func jobSuffix(req *Request) string {
	if isArchive(req) {
		return "tar"
	}

	return "rclone"
}
