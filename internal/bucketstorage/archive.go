package bucketstorage

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/utkuozdemir/pv-migrate/internal/archive"
	"github.com/utkuozdemir/pv-migrate/internal/k8s"
	"github.com/utkuozdemir/pv-migrate/internal/pvc"
	"github.com/utkuozdemir/pv-migrate/internal/rclone"
)

// archiveFormat is recorded in the metadata sidecar so a reader knows what the
// archive file is without inspecting it.
const archiveFormat = "tar"

// archiveSuffixes are the extensions an archive file may carry, longest first.
var archiveSuffixes = []string{".tar.zst", ".tzst", ".tar.gz", ".tgz", ".tar"}

// isArchive reports whether the request asks for the archive workflow.
func isArchive(req *Request) bool {
	return req.ArchiveFile != ""
}

// parseArchiveTarget reads --archive-file and rejects a request that also names
// a bucket, which would be a second destination for the same data.
func parseArchiveTarget(req *Request, now time.Time) (archive.Target, error) {
	target, err := archive.ParseTarget(req.ArchiveFile, now)
	if err != nil {
		return archive.Target{}, err
	}

	if err := checkArchiveConflicts(req, target); err != nil {
		return archive.Target{}, err
	}

	if req.CompressionLevel != 0 {
		// A level only shapes compression. A restore decompresses, and the
		// level that made the archive is already fixed in the file.
		if req.Direction == rclone.DirectionRestore {
			return archive.Target{}, errors.New("--compression-level does not apply to a restore")
		}

		if target.Compression == archive.CompressionNone {
			return archive.Target{}, fmt.Errorf(
				"--compression-level does not apply to %q, whose extension asks for no compression",
				target.Path)
		}

		// Checked now rather than when the command is built, which for a
		// snapshot-backed run would be after the snapshot cycle.
		if err := archive.CheckLevel(target.Compression, req.CompressionLevel); err != nil {
			return archive.Target{}, err
		}
	}

	// Writing the archive into the volume being read would feed tar its own
	// output, so this is refused before anything is looked up in the cluster.
	if target.Claim == req.PVCName {
		return archive.Target{}, errors.New(
			"--archive-file names the claim being backed up or restored")
	}

	return target, nil
}

// checkArchiveConflicts refuses the bucket flags that would name a second
// destination. The s3:// form takes its credentials from the S3 flags, so only
// the flags naming a location are refused there; the other forms have no
// bucket at all, so every bucket flag is a conflict.
func checkArchiveConflicts(req *Request, target archive.Target) error {
	type conflict struct {
		flag  string
		isSet bool
	}

	conflicts := []conflict{
		{"--bucket", req.Bucket != ""},
		{"--rclone-config", req.RcloneConfigFile != ""},
		{"--remote", req.Remote != ""},
		// The prefix arrives defaulted from the public API and the CLI, so
		// only a value someone chose counts.
		{"--prefix", req.Prefix != "" && req.Prefix != DefaultPrefix},
		{"--name", req.Name != ""},
	}

	if target.InBucket() {
		if req.Backend != "" && req.Backend != rclone.BackendS3 {
			return fmt.Errorf(
				"--archive-file with s3:// is an S3 destination, so --backend %q does not apply", req.Backend)
		}
	} else {
		// The file forms run no rclone, so its flags have nothing to act on.
		conflicts = append(conflicts,
			conflict{"--backend", req.Backend != ""},
			conflict{"--rclone-extra-args", req.RcloneExtraArgs != ""},
		)
	}

	for _, conflict := range conflicts {
		if conflict.isSet {
			return fmt.Errorf("%s cannot be combined with --archive-file, "+
				"which names the destination itself", conflict.flag)
		}
	}

	return nil
}

// archiveFilePath resolves the target's path against the mount the job sees.
//
// A path on a claim is read as claim-root-relative whether or not it leads with
// a slash, and is checked for staying inside that root before anything touches
// the cluster: without the check, "../" would name the container's own
// filesystem, which tar would then write to.
func archiveFilePath(target archive.Target) (string, error) {
	if !target.InCluster() {
		return target.Path, nil
	}

	cleaned := path.Clean(strings.TrimPrefix(target.Path, "/"))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned == "." {
		return "", fmt.Errorf("--archive-file path %q must name a file inside the claim", target.Path)
	}

	return path.Join(archiveMountPath, cleaned), nil
}

// archiveDirection maps the request's direction onto the archive package's.
func archiveDirection(req *Request) string {
	if req.Direction == rclone.DirectionRestore {
		return archive.DirectionRestore
	}

	return archive.DirectionBackup
}

// buildArchiveCmd builds the tar command for the request's direction.
func buildArchiveCmd(req *Request, target archive.Target, dataPath string) (string, error) {
	archivePath, err := archiveFilePath(target)
	if err != nil {
		return "", err
	}

	cmd := archive.Cmd{
		Direction:   archiveDirection(req),
		ArchivePath: archivePath,
		DataPath:    dataPath,
		Compression: target.Compression,
		Level:       req.CompressionLevel,
		Clean:       req.DeleteExtraneousFiles,
	}

	return cmd.Build()
}

// resolveArchiveClaim looks up the claim holding the archive and checks that one
// pod can mount it alongside the claim being backed up or restored.
func resolveArchiveClaim(
	ctx context.Context,
	client *k8s.ClusterClient,
	namespace string,
	target archive.Target,
	dataInfo *pvc.Info,
) (*pvc.Info, error) {
	archiveInfo, err := pvc.New(ctx, client, namespace, target.Claim)
	if err != nil {
		return nil, fmt.Errorf("failed to get archive PVC info: %w", err)
	}

	if err = checkSchedulable(dataInfo, archiveInfo); err != nil {
		return nil, err
	}

	return archiveInfo, nil
}

// placementNodes returns the nodes a job mounting both claims could run on,
// or nil when nothing constrains it.
//
// Each claim contributes what its own volume permits, which the CSI driver
// published as that volume's topology: one node for node-local storage, every
// node in a zone for a cloud disk, nothing at all for storage reachable from
// anywhere. A job has to satisfy both at once, so the answer is the
// intersection.
func placementNodes(dataInfo, archiveInfo *pvc.Info) []string {
	return pvc.IntersectNodes(dataInfo.PinnedNodes, archiveInfo.PinnedNodes)
}

// checkSchedulable reports whether one pod could mount both claims.
//
// The volumes decide this, not the pods currently using them. A volume carries
// its topology whether or not anything has it mounted, so a freshly cloned
// claim that no pod has ever touched can still be pinned to the node its
// snapshot lives on. Saying so here is the difference between an explanation
// and a pod that sits Pending until the install times out.
func checkSchedulable(dataInfo, archiveInfo *pvc.Info) error {
	if nodes := placementNodes(dataInfo, archiveInfo); nodes == nil || len(nodes) > 0 {
		return nil
	}

	return fmt.Errorf(
		"claim %s can only be used on %s and claim %s only on %s, "+
			"so one pod cannot mount both; their storage is bound to those nodes",
		dataInfo.Claim.Name, describeNodes(dataInfo.PinnedNodes),
		archiveInfo.Claim.Name, describeNodes(archiveInfo.PinnedNodes),
	)
}

// describeNodes renders a node set for an error message.
func describeNodes(nodes []string) string {
	if len(nodes) == 0 {
		return "no node"
	}

	return strings.Join(nodes, ", ")
}

// archiveAffinity returns the node affinity that satisfies both claims, which
// is their intersection when either is constrained.
func archiveAffinity(dataInfo, archiveInfo *pvc.Info) map[string]any {
	if nodes := placementNodes(dataInfo, archiveInfo); nodes != nil {
		return pvc.RequireNodes(nodes)
	}

	// Neither volume is pinned, so the only hint left is where each claim
	// happens to be mounted, which each claim already expressed as a
	// preference.
	if dataInfo.AffinityHelmValues != nil {
		return dataInfo.AffinityHelmValues
	}

	return archiveInfo.AffinityHelmValues
}

// resolveTarget produces the rclone config and the path the data mover reads or
// writes, for whichever of the two workflows the request selects. An archive
// needs no config, so its config is empty.
func resolveTarget(req *Request, target archive.Target) (string, string, error) {
	if isArchive(req) {
		if target.InBucket() {
			return resolveBucketArchive(req, target)
		}

		archivePath, err := archiveFilePath(target)

		return "", archivePath, err
	}

	rcloneConf, err := buildRcloneConfig(req)
	if err != nil {
		return "", "", fmt.Errorf("failed to build rclone config: %w", err)
	}

	remotePath, err := buildRemotePath(req)
	if err != nil {
		return "", "", err
	}

	return rcloneConf, remotePath, nil
}

// buildMoverCmd builds the command the job runs, tar or rclone.
func buildMoverCmd(req *Request, target archive.Target, localPath, remotePath string) (string, error) {
	if isArchive(req) && target.InBucket() {
		streamCmd := archive.StreamCmd{
			Direction:   archiveDirection(req),
			RemotePath:  remotePath,
			DataPath:    localPath,
			ConfigPath:  rcloneConfigMountPath,
			Compression: target.Compression,
			Level:       req.CompressionLevel,
			ExtraArgs:   req.RcloneExtraArgs,
			Clean:       req.DeleteExtraneousFiles,
		}

		cmdStr, err := streamCmd.Build()
		if err != nil {
			return "", fmt.Errorf("failed to build streamed tar command: %w", err)
		}

		return cmdStr, nil
	}

	if isArchive(req) {
		cmdStr, err := buildArchiveCmd(req, target, localPath)
		if err != nil {
			return "", fmt.Errorf("failed to build tar command: %w", err)
		}

		return cmdStr, nil
	}

	rcloneCmd := rclone.Cmd{
		Direction:  req.Direction,
		RemotePath: remotePath,
		LocalPath:  localPath,
		ConfigPath: rcloneConfigMountPath,
		ExtraArgs:  req.RcloneExtraArgs,
		Delete:     req.DeleteExtraneousFiles,
	}

	cmdStr, err := rcloneCmd.Build()
	if err != nil {
		return "", fmt.Errorf("failed to build rclone command: %w", err)
	}

	return cmdStr, nil
}

// buildMetadata renders the sidecar and decides where it goes. A restore writes
// none, and neither does a backup whose destination the tool does not manage.
func buildMetadata(req *Request, target archive.Target, namespace string) (metadataValues, error) {
	var meta metadataValues

	if req.Direction != rclone.DirectionBackup {
		return meta, nil
	}

	switch {
	case isArchive(req):
		encoded, err := generateMetadataBase64(namespace, req.PVCName, archiveFormat, target.Compression)
		if err != nil {
			return meta, fmt.Errorf("failed to generate backup metadata: %w", err)
		}

		meta.base64 = encoded

		// An S3 archive's sidecar is uploaded next to the object through the
		// same path a bucket backup uses; the others write it next to the file.
		// A dry run uploads nothing, so it writes no sidecar either.
		if target.InBucket() {
			if hasRcloneDryRun(req.RcloneExtraArgs) {
				return metadataValues{}, nil
			}

			meta.remotePath = rclone.BuildObjectPath(target.Bucket, sidecarPath(target.Path))

			return meta, nil
		}

		archivePath, err := archiveFilePath(target)
		if err != nil {
			return meta, err
		}

		meta.localPath = sidecarPath(archivePath)
	case shouldUploadMetadata(req):
		encoded, err := generateMetadataBase64(namespace, req.PVCName, "", "")
		if err != nil {
			return meta, fmt.Errorf("failed to generate backup metadata: %w", err)
		}

		meta.base64 = encoded
		meta.remotePath = rclone.BuildMetadataRemotePath(req.Bucket, req.Prefix, req.Name)
	}

	return meta, nil
}

// sidecarPath puts the metadata next to the archive, named after it with the
// archive's extension replaced, so the pair is obvious in a directory listing
// and one backup's metadata does not overwrite another's.
func sidecarPath(archivePath string) string {
	for _, ext := range archiveSuffixes {
		if stem, found := strings.CutSuffix(archivePath, ext); found {
			return stem + metadataSuffix
		}
	}

	return archivePath + metadataSuffix
}

// resolveBucketArchive builds the rclone config and remote path for an archive
// streamed to S3. The S3 credential flags are reused as they are; only the
// backend is fixed, since the s3:// scheme already said which one it is.
func resolveBucketArchive(req *Request, target archive.Target) (string, string, error) {
	s3Req := *req
	s3Req.Backend = rclone.BackendS3

	rcloneConf, err := buildRcloneConfig(&s3Req)
	if err != nil {
		return "", "", fmt.Errorf("failed to build rclone config: %w", err)
	}

	if err := validateBucketSegment(target.Bucket, "bucket"); err != nil {
		return "", "", err
	}

	return rcloneConf, rclone.BuildObjectPath(target.Bucket, target.Path), nil
}
