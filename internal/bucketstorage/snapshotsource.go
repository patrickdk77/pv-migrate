package bucketstorage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/utkuozdemir/pv-migrate/internal/flush"
	"github.com/utkuozdemir/pv-migrate/internal/k8s"
	"github.com/utkuozdemir/pv-migrate/internal/narrate"
	"github.com/utkuozdemir/pv-migrate/internal/pvc"
	"github.com/utkuozdemir/pv-migrate/internal/rclone"
	"github.com/utkuozdemir/pv-migrate/internal/snapshot"
)

const (
	// cutTimeout bounds the wait for the storage system to cut the snapshot.
	// A database lock is held for this whole wait, so it is kept short.
	cutTimeout = 5 * time.Minute
	// readyTimeout bounds the wait for readyToUse, which some drivers reach
	// only after post-processing. No lock is held here.
	readyTimeout = 30 * time.Minute
	// markerTimeout bounds how long a database may take to confirm its lock.
	markerTimeout = 30 * time.Second

	snapshotSuffix = "-snap"
	cloneSuffix    = "-clone"
)

// snapshotSource is what a backup reads when told to work from a snapshot: the
// clone to mount in place of the live claim, and how to tear it down after.
type snapshotSource struct {
	clone   *pvc.Info
	cleanup func(context.Context) error
}

// usesSnapshot reports whether the request backs up a point-in-time copy
// rather than the live claim.
func usesSnapshot(req *Request) bool {
	return req.Snapshot || req.FromSnapshot != "" || req.Flush != ""
}

// validateSnapshotRequest rejects the combinations that cannot mean anything.
func validateSnapshotRequest(req *Request) error {
	if !usesSnapshot(req) {
		return nil
	}

	if req.Direction != rclone.DirectionBackup {
		return errors.New("--snapshot, --from-snapshot and --flush apply to a backup, not a restore")
	}

	if req.FromSnapshot != "" && req.Flush != "" {
		return errors.New("--flush quiesces the database while a new snapshot is cut, " +
			"so it cannot be combined with --from-snapshot, which reads one that already exists")
	}

	if req.Flush != "" {
		if _, err := flush.Lookup(req.Flush); err != nil {
			return err
		}
	}

	return nil
}

// resolveSource decides what the job mounts. A snapshot-backed run reads a
// clone, so the live claim being mounted is expected rather than a conflict,
// since it is the running database. The returned release tears the clone down
// and is a no-op for a plain run, so the caller's defer reads the same either
// way.
func resolveSource(
	ctx context.Context,
	client *k8s.ClusterClient,
	req *Request,
	live *pvc.Info,
	releaseName string,
	details, logger *slog.Logger,
) (*pvc.Info, func(failed bool), error) {
	if !usesSnapshot(req) {
		if err := handleMounted(live, req.IgnoreMounted, details); err != nil {
			return nil, nil, err
		}

		return live, func(bool) {}, nil
	}

	src, err := prepareSnapshotSource(ctx, client, req, live, releaseName, logger)
	if err != nil {
		return nil, nil, err
	}

	return src.clone, func(failed bool) { releaseSnapshotSource(ctx, req, src, failed, logger) }, nil
}

// prepareSnapshotSource runs the sequence a consistent backup needs and hands
// back the clone the backup will read:
//
//  1. If --flush is set, open a client session in the database's pod and take
//     its lock, confirmed by a marker the client prints back.
//  2. Create the VolumeSnapshot, unless --from-snapshot named an existing one.
//  3. Wait for the storage system to cut it, which is the moment its content
//     carries a handle. This is where the data is fixed.
//  4. Release the lock. It was held for the cut only, not the backup.
//  5. Wait for readyToUse, which the provisioner requires before cloning.
//  6. Clone a claim from the snapshot and return it.
func prepareSnapshotSource(
	ctx context.Context,
	client *k8s.ClusterClient,
	req *Request,
	source *pvc.Info,
	releaseName string,
	logger *slog.Logger,
) (result *snapshotSource, retErr error) {
	details := narrate.Detail(logger, 1)
	namespace := source.Claim.Namespace

	snapClient, err := snapshot.New(client.RestConfig, client.KubeClient)
	if err != nil {
		return nil, err
	}

	// Checked before the database is quiesced. A class that does not exist
	// would otherwise only surface when the cut times out, with the lock held
	// for that whole wait.
	if req.FromSnapshot == "" {
		if err = snapClient.CheckClass(ctx, req.SnapshotClass); err != nil {
			return nil, err
		}
	}

	labels := runLabels(releaseName)

	snapshotName, created, err := ensureSnapshot(ctx, client, snapClient, req, source, releaseName, details)
	if err != nil {
		return nil, err
	}

	removeSnapshot := created && !req.KeepSnapshot
	cloneName := releaseName + cloneSuffix
	cloneCreated := false

	// Nothing below is reachable by pv-migrate cleanup, which finds a run
	// through its Helm release and none exists yet, so a failure here has to
	// take back what it made.
	defer func() {
		if retErr != nil {
			rollbackSnapshotSource(context.WithoutCancel(ctx), snapClient, namespace,
				cloneName, snapshotName, cloneCreated, removeSnapshot, details)
		}
	}()

	if err = snapClient.WaitReady(ctx, namespace, snapshotName, readyTimeout); err != nil {
		return nil, err
	}

	var clone *pvc.Info

	clone, cloneCreated, err = cloneSnapshot(ctx, client, snapClient, snapshotName, cloneName, source, labels, details)
	if err != nil {
		return nil, err
	}

	return &snapshotSource{
		clone: clone,
		cleanup: func(ctx context.Context) error {
			return removeSnapshotSource(ctx, snapClient, namespace, cloneName, snapshotName, removeSnapshot, details)
		},
	}, nil
}

// ensureSnapshot returns the snapshot to clone from: the one --from-snapshot
// named, or a new one cut under the database lock. created says which, since
// only a snapshot this run made is its to remove.
func ensureSnapshot(
	ctx context.Context,
	client *k8s.ClusterClient,
	snapClient *snapshot.Client,
	req *Request,
	source *pvc.Info,
	releaseName string,
	details *slog.Logger,
) (string, bool, error) {
	if req.FromSnapshot != "" {
		details.Info("📸 using existing snapshot " + req.FromSnapshot)

		return req.FromSnapshot, false, nil
	}

	snapshotName := releaseName + snapshotSuffix

	err := cutSnapshot(ctx, client, snapClient, req, source, snapshotName,
		snapshotLabels(releaseName, req.KeepSnapshot), details)
	if err != nil {
		return "", false, err
	}

	return snapshotName, true, nil
}

// cloneSnapshot provisions a claim from the snapshot and resolves it. created
// is true from the moment the claim exists, so a failed lookup afterwards
// still leaves the caller a clone to roll back.
func cloneSnapshot(
	ctx context.Context,
	client *k8s.ClusterClient,
	snapClient *snapshot.Client,
	snapshotName, cloneName string,
	source *pvc.Info,
	labels map[string]string,
	details *slog.Logger,
) (clone *pvc.Info, created bool, err error) {
	namespace := source.Claim.Namespace

	if err = snapClient.Clone(ctx, snapshotName, cloneName, source.Claim, labels); err != nil {
		return nil, false, err
	}

	details.Info(fmt.Sprintf("📸 cloned claim %s from snapshot %s", cloneName, snapshotName))

	clone, err = pvc.New(ctx, client, namespace, cloneName)
	if err != nil {
		return nil, true, fmt.Errorf("failed to get cloned claim: %w", err)
	}

	inheritTopology(clone, source)

	return clone, true, nil
}

// inheritTopology gives a clone the source volume's topology while the clone
// has none of its own.
//
// A clone bound straight away publishes its own affinity and that is used as
// is. One provisioned on first consumer has no volume yet, so nothing can be
// read from it, and treating that as "goes anywhere" is how a backup ends up
// waiting on a pod that can never be scheduled.
//
// The source is the right answer to fall back on: a snapshot is taken of that
// volume and can be restored only where the driver can place it, which is at
// most where the volume itself could live. A volume pinned to one node yields
// a snapshot restorable on that node; one pinned to an availability zone
// yields a snapshot restorable in that zone.
func inheritTopology(clone, source *pvc.Info) {
	if clone.AllowedNodes != nil || source.AllowedNodes == nil {
		return
	}

	clone.AllowedNodes = source.AllowedNodes
	clone.PinnedNodes = source.AllowedNodes
	clone.AffinityHelmValues = pvc.RequireNodes(source.AllowedNodes)
}

// rollbackSnapshotSource removes what a failed prepareSnapshotSource made,
// each on a best-effort basis, since the failure being reported is the one
// that matters.
func rollbackSnapshotSource(
	ctx context.Context,
	snapClient *snapshot.Client,
	namespace, cloneName, snapshotName string,
	cloneCreated, removeSnapshot bool,
	details *slog.Logger,
) {
	if cloneCreated {
		if err := snapClient.DeleteClaim(ctx, namespace, cloneName); err != nil {
			details.Warn("🔶 could not remove cloned claim " + cloneName + ": " + err.Error())
		}
	}

	if removeSnapshot {
		if err := snapClient.DeleteSnapshot(ctx, namespace, snapshotName); err != nil {
			details.Warn("🔶 could not remove snapshot " + snapshotName + ": " + err.Error())
		}
	}
}

// runLabels are what every resource of a run carries, and what cleanup finds
// it by.
func runLabels(releaseName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "pv-migrate",
		"app.kubernetes.io/instance": releaseName,
	}
}

// snapshotLabels are the run labels for the snapshot, minus the instance label
// when the snapshot is to be kept, so that a later cleanup of the run does not
// take it along with everything else.
func snapshotLabels(releaseName string, keep bool) map[string]string {
	if keep {
		return map[string]string{"app.kubernetes.io/name": "pv-migrate"}
	}

	return runLabels(releaseName)
}

// removeSnapshotSource deletes the clone and, when the run created the
// snapshot and was not told to keep it, the snapshot too. The clone goes first
// since it holds the snapshot in use.
func removeSnapshotSource(
	ctx context.Context,
	snapClient *snapshot.Client,
	namespace, cloneName, snapshotName string,
	removeSnapshot bool,
	details *slog.Logger,
) error {
	if err := snapClient.DeleteClaim(ctx, namespace, cloneName); err != nil {
		return err
	}

	details.Info("🧹 removed cloned claim " + cloneName)

	if !removeSnapshot {
		return nil
	}

	if err := snapClient.DeleteSnapshot(ctx, namespace, snapshotName); err != nil {
		return err
	}

	details.Info("🧹 removed snapshot " + snapshotName)

	return nil
}

// cutSnapshot creates the snapshot under the database lock, if one was asked
// for, and releases the lock the moment the cut is confirmed. The release is
// deferred from the moment the lock is taken, so a failure anywhere after it
// still releases the database, and its own error is kept when nothing else
// went wrong, since a database left locked is worse than a backup that did
// not happen.
func cutSnapshot(
	ctx context.Context,
	client *k8s.ClusterClient,
	snapClient *snapshot.Client,
	req *Request,
	source *pvc.Info,
	snapshotName string,
	labels map[string]string,
	details *slog.Logger,
) (retErr error) {
	namespace := source.Claim.Namespace

	unlock, err := quiesce(ctx, client, req, source, details)
	if err != nil {
		return err
	}

	defer func() {
		if unlockErr := unlock(); unlockErr != nil && retErr == nil {
			retErr = unlockErr
		}
	}()

	err = snapClient.Create(ctx, namespace, source.Claim.Name, req.SnapshotClass, snapshotName, labels)
	if err != nil {
		return err
	}

	details.Info(fmt.Sprintf("📸 created snapshot %s of claim %s", snapshotName, source.Claim.Name))

	// Whether the snapshot itself cut, tracked separately from the function's
	// error. A failure to release the database lock is reported, but it says
	// nothing about the snapshot and must not throw a good one away.
	cut := false

	// A snapshot that never cut is not a snapshot, and no release exists yet
	// for pv-migrate cleanup to find it by, so this path takes it back itself.
	// That holds even under --keep-snapshot, which asks to keep a usable one.
	defer func() {
		if cut {
			return
		}

		if err := snapClient.DeleteSnapshot(context.WithoutCancel(ctx), namespace, snapshotName); err != nil {
			details.Warn("🔶 could not remove the snapshot that failed to cut, " +
				"remove " + snapshotName + " by hand: " + err.Error())

			return
		}

		details.Info("🧹 removed snapshot " + snapshotName + ", which never cut")
	}()

	if err = snapClient.WaitForCut(ctx, namespace, snapshotName, cutTimeout, details); err != nil {
		return err
	}

	cut = true

	return unlock()
}

// quiesce takes the database's backup lock when --flush is set and returns a
// function that releases it. Without --flush the returned function does
// nothing, so the caller's sequence reads the same either way. The release is
// idempotent, so the deferred call after an explicit one is a no-op.
func quiesce(
	ctx context.Context,
	client *k8s.ClusterClient,
	req *Request,
	source *pvc.Info,
	details *slog.Logger,
) (func() error, error) {
	if req.Flush == "" {
		return func() error { return nil }, nil
	}

	plan, err := prepareFlush(ctx, client, req, source)
	if err != nil {
		return nil, err
	}

	spec, target, creds := plan.spec, plan.target, plan.creds

	var (
		release func() error
		note    = spec.Note
	)

	switch spec.Mode {
	case flush.ModeSession:
		release, note, err = quiesceSession(ctx, target, req.Flush, spec, creds)
	case flush.ModeOneShot:
		release, err = quiesceOneShot(ctx, target, req.Flush, spec, creds)
	case flush.ModePair:
		release, err = quiescePair(ctx, target, req.Flush, spec, creds)
	default:
		return nil, fmt.Errorf("--flush %s has an unknown mode %d", req.Flush, spec.Mode)
	}

	if err != nil {
		return nil, err
	}

	lockedAt := time.Now()

	details.Info(fmt.Sprintf("🔒 %s quiesced in pod %s: %s", req.Flush, source.MountedPod, note))

	released := false

	return func() error {
		if released {
			return nil
		}

		released = true

		if err := release(); err != nil {
			return err
		}

		details.Info(fmt.Sprintf("🔓 released %s after %s", req.Flush,
			time.Since(lockedAt).Round(time.Millisecond)))

		return nil
	}, nil
}

// flushPlan is what a flush runs, where, and as whom.
type flushPlan struct {
	spec   flush.Spec
	target execTarget
	creds  *flush.Credentials
}

// prepareFlush resolves the kind's recipe, applies a --flush-command
// override, works out the credentials, and finds where the commands run: the
// pod that has the claim mounted, which for a database volume is the database.
func prepareFlush(
	ctx context.Context, client *k8s.ClusterClient, req *Request, source *pvc.Info,
) (flushPlan, error) {
	spec, err := flush.Lookup(req.Flush)
	if err != nil {
		return flushPlan{}, err
	}

	if source.MountedPod == "" {
		return flushPlan{}, fmt.Errorf(
			"--flush %s needs a database to talk to, but no pod has claim %s mounted",
			req.Flush, source.Claim.Name)
	}

	if len(req.FlushCommand) > 0 {
		// A pair kind runs three commands, and replacing only the first would
		// lock with the user's client and unlock with the built-in one.
		if spec.Mode == flush.ModePair {
			return flushPlan{}, fmt.Errorf(
				"--flush-command replaces the one client a kind runs, and %s runs three "+
					"(lock, unlock and a check), so it takes none", req.Flush)
		}

		spec = spec.WithCommand(req.FlushCommand)
	}

	creds, err := resolveFlushCredentials(ctx, client, req, source.Claim.Namespace, spec)
	if err != nil {
		return flushPlan{}, err
	}

	return flushPlan{
		spec: spec,
		target: execTarget{
			client: client, namespace: source.Claim.Namespace,
			pod: source.MountedPod, container: req.FlushContainer,
		},
		creds: creds,
	}, nil
}

// resolveFlushCredentials returns who the flush client logs in as, or nil
// when the command reads no credentials.
//
// Credentials given to a command that cannot use them are refused rather than
// dropped. A backup that ignored --flush-user would log in as whoever the
// image seeds and either fail with an error naming the wrong user, or worse,
// succeed as someone the operator did not intend.
func resolveFlushCredentials(
	ctx context.Context, client *k8s.ClusterClient, req *Request, namespace string, spec flush.Spec,
) (*flush.Credentials, error) {
	if !spec.ReadsCredentials {
		return nil, refuseUnusedCredentials(req)
	}

	if req.FlushPassword != "" && req.FlushPasswordSecret != "" {
		return nil, errors.New("give the flush password one way, either directly or " +
			"with --flush-password-secret, not both")
	}

	creds := &flush.Credentials{User: req.FlushUser, Password: req.FlushPassword}

	if req.FlushPasswordSecret != "" {
		password, err := readSecretValue(ctx, client, namespace, req.FlushPasswordSecret)
		if err != nil {
			return nil, err
		}

		creds.Password = password
	}

	if err := creds.Check(); err != nil {
		return nil, err
	}

	return creds, nil
}

// refuseUnusedCredentials allows a command that reads no credentials to run
// only when none were given.
func refuseUnusedCredentials(req *Request) error {
	switch {
	case req.FlushUser == "" && req.FlushPassword == "" && req.FlushPasswordSecret == "":
		return nil
	case len(req.FlushCommand) > 0:
		return errors.New("--flush-command replaces the built-in client, so " +
			"--flush-user and the flush password have nothing to apply to; " +
			"log in from the command itself, or drop --flush-command")
	default:
		return fmt.Errorf("--flush %s logs in as nobody: nodetool reaches the node through its "+
			"local REST API, not CQL, so --flush-user and the flush password do not apply", req.Flush)
	}
}

// flushSecretDefaultKey is the key read when --flush-password-secret names
// only a Secret.
const flushSecretDefaultKey = "password"

// readSecretValue reads one key of a Secret, named "name" or "name:key". The
// Secret lives in the database's namespace, which is where an operator such
// as Percona's keeps it, and where a CronJob in another namespace could not
// reach it with a secretKeyRef.
//
// No error here quotes a value; a missing key lists the keys that exist.
func readSecretValue(ctx context.Context, client *k8s.ClusterClient, namespace, ref string) (string, error) {
	name, key, hasKey := strings.Cut(ref, ":")
	if !hasKey {
		key = flushSecretDefaultKey
	}

	if name == "" || key == "" {
		return "", fmt.Errorf("--flush-password-secret %q must be a Secret name, or name:key", ref)
	}

	secret, err := client.KubeClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to read the flush password from secret %s/%s: %w", namespace, name, err)
	}

	value, ok := secret.Data[key]
	if !ok {
		keys := make([]string, 0, len(secret.Data))
		for k := range secret.Data {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		return "", fmt.Errorf("secret %s/%s has no key %q; it has: %s",
			namespace, name, key, strings.Join(keys, ", "))
	}

	return string(value), nil
}

// execTarget is where a flush command runs.
type execTarget struct {
	client                    *k8s.ClusterClient
	namespace, pod, container string
}

// run execs command. With credentials it sends the password as the first
// line of stdin, which is where every built-in command reads it from.
func (t execTarget) run(ctx context.Context, command []string, creds *flush.Credentials) (string, error) {
	var password *string
	if creds != nil {
		password = &creds.Password
	}

	return flush.Run(ctx, t.client.RestConfig, t.client.KubeClient,
		t.namespace, t.pod, t.container, command, password)
}

func userOf(creds *flush.Credentials) string {
	if creds == nil {
		return ""
	}

	return creds.User
}

// quiesceSession starts the client, asks the server what it is, chooses the
// lock that server supports, sends it, and waits for the marker that proves
// it executed. The returned release sends the matching unlock and lets the
// client exit. The note returned names the server and the lock, since which
// one was taken decides what the database went through.
func quiesceSession(
	ctx context.Context, target execTarget, kind string, spec flush.Spec, creds *flush.Credentials,
) (func() error, string, error) {
	session, err := flush.Open(ctx, target.client.RestConfig, target.client.KubeClient,
		target.namespace, target.pod, target.container, spec.Command(userOf(creds)))
	if err != nil {
		return nil, "", err
	}

	fail := func(err error) (func() error, string, error) {
		_ = session.Close()

		return nil, "", err
	}

	if creds != nil {
		if err = session.WriteSecret(creds.Password); err != nil {
			return fail(err)
		}
	}

	lock, server, err := chooseLock(ctx, session, kind, spec)
	if err != nil {
		return fail(err)
	}

	for _, statement := range []string{lock.Statement, spec.Probe} {
		if err = session.Write(statement); err != nil {
			return fail(err)
		}
	}

	if err = session.WaitFor(ctx, spec.Marker, markerTimeout); err != nil {
		return fail(fmt.Errorf("failed to confirm the %s lock (%s): %w", kind, lock.Name, err))
	}

	note := fmt.Sprintf("%s on %s; %s", lock.Name, server, lock.Note)

	return func() error {
		if err := session.Write(lock.Unlock); err != nil {
			_ = session.Close()

			return err
		}

		if err := session.Close(); err != nil {
			return fmt.Errorf("the %s unlock was sent but the client did not exit cleanly: %w", kind, err)
		}

		return nil
	}, note, nil
}

// chooseLock asks the server what it is and chooses the lock it supports. It
// returns the server's version too, for the note.
func chooseLock(
	ctx context.Context, session *flush.Session, kind string, spec flush.Spec,
) (flush.Lock, string, error) {
	if err := session.Write(spec.Identify); err != nil {
		return flush.Lock{}, "", err
	}

	if err := session.WaitFor(ctx, flush.IdentityEnd, markerTimeout); err != nil {
		return flush.Lock{}, "", fmt.Errorf("--flush %s could not ask the server what it is: %w", kind, err)
	}

	identity, err := flush.ParseIdentity(session.Output())
	if err != nil {
		return flush.Lock{}, "", fmt.Errorf("--flush %s: %w", kind, err)
	}

	lock, err := spec.Choose(identity)
	if err != nil {
		return flush.Lock{}, "", fmt.Errorf("--flush %s: %w", kind, err)
	}

	server, _, _ := strings.Cut(identity, "|")

	return lock, server, nil
}

// quiesceOneShot runs the command and holds nothing, so there is nothing to
// release.
func quiesceOneShot(
	ctx context.Context, target execTarget, kind string, spec flush.Spec, creds *flush.Credentials,
) (func() error, error) {
	if out, err := target.run(ctx, spec.Command(userOf(creds)), creds); err != nil {
		return nil, fmt.Errorf("--flush %s failed: %w; output: %s", kind, err, out)
	}

	return func() error { return nil }, nil
}

// quiescePair runs the lock command now and the release command later, then
// checks the release took. The check exists because a lock the database
// counts, rather than owns per connection, can be left over from an earlier
// run, and one unlock would then leave it locked while reporting success.
func quiescePair(
	ctx context.Context, target execTarget, kind string, spec flush.Spec, creds *flush.Credentials,
) (func() error, error) {
	out, err := target.run(ctx, spec.Command(userOf(creds)), creds)
	if err != nil {
		return nil, fmt.Errorf("--flush %s failed: %w; output: %s", kind, err, out)
	}

	if err = checkLockTaken(kind, spec, out); err != nil {
		return nil, err
	}

	// The release has to run even after the run's context is cancelled by a
	// signal, since the lock lives on the server and nothing else clears it.
	releaseCtx := context.WithoutCancel(ctx)

	return func() error {
		out, err := target.run(releaseCtx, spec.Release(userOf(creds)), creds)
		if err != nil {
			return fmt.Errorf("the %s release failed and the database may still be locked: %w; output: %s",
				kind, err, out)
		}

		if spec.ReleaseMarker != "" && !strings.Contains(out, spec.ReleaseMarker) {
			return fmt.Errorf("the %s release ran but the database is still locked, so a lock leaked by "+
				"an earlier run is still held; release it by hand before the next backup; output: %s",
				kind, out)
		}

		return nil
	}, nil
}

// checkLockTaken reads the lock command's output for LockMarker, since
// exiting 0 does not mean the lock was taken. A refused lock holds nothing, so
// there is nothing to release either.
func checkLockTaken(kind string, spec flush.Spec, out string) error {
	if spec.LockMarker != "" && !strings.Contains(out, spec.LockMarker) {
		return fmt.Errorf("--flush %s: the database refused the lock, so the snapshot would not "+
			"be protected; output: %s", kind, out)
	}

	return nil
}

// releaseSnapshotSource tears the clone and snapshot down once the backup is
// over, except where the run asked to leave things in place. A detached job is
// still reading the clone, so removing it would pull the volume out from under
// the transfer; pv-migrate cleanup finds both by their label later.
func releaseSnapshotSource(
	ctx context.Context, req *Request, src *snapshotSource, failed bool, logger *slog.Logger,
) {
	details := narrate.Detail(logger, 1)

	switch {
	case req.Detach:
		details.Info("📸 the job is still using the cloned claim; pv-migrate cleanup removes it and the snapshot")

		return
	case req.NoCleanup, failed && req.NoCleanupOnFailure:
		details.Info("📸 leaving the snapshot and cloned claim in place; pv-migrate cleanup removes them")

		return
	}

	if err := src.cleanup(context.WithoutCancel(ctx)); err != nil {
		// The release is gone by now, so cleanup cannot find these; naming
		// them is the only help left to give.
		details.Warn(fmt.Sprintf("🔶 could not remove the snapshot clone: %s; "+
			"remove claim %s and its VolumeSnapshot by hand", err, src.clone.Claim.Name))
	}
}
