//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/utkuozdemir/pv-migrate/internal/k8s"
	"github.com/utkuozdemir/pv-migrate/pvmigrate"
)

// The snapshot API is a set of CRDs, so a cluster can be any Kubernetes
// version and still not have it. These tests need the CRDs, their controller,
// and a CSI driver whose storage class supports snapshots, which the default
// local-path provisioner does not.
const (
	envSnapshotClass   = "PVMIG_TEST_SNAPSHOT_CLASS"
	envSnapshotMustRun = "PVMIG_TEST_SNAPSHOT_MUST_RUN"
)

var (
	volumeSnapshotGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshots",
	}
	volumeSnapshotClassGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshotclasses",
	}
)

// snapshotInfra carries what the snapshot tests need beyond the backup suite:
// a dynamic client for the CRDs and the class to cut with.
type snapshotInfra struct {
	*backupTestInfra

	dyn   dynamic.Interface
	class string
}

// setupSnapshotInfra skips the test unless the cluster can actually take a
// snapshot, since a missing CRD is a property of the cluster rather than a
// failure of the code under test.
func setupSnapshotInfra(t *testing.T) *snapshotInfra {
	t.Helper()

	infra := setupBackupInfra(t)

	dyn, err := dynamic.NewForConfig(infra.cli.RestConfig)
	require.NoError(t, err)

	class := os.Getenv(envSnapshotClass)
	if class == "" {
		skipOrFail(t, envSnapshotClass+" not set, so the cluster has no snapshot class to cut with",
			envSnapshotMustRun)
	}

	if _, err = dyn.Resource(volumeSnapshotClassGVR).Get(t.Context(), class, metav1.GetOptions{}); err != nil {
		skipOrFail(t, "VolumeSnapshotClass "+class+" is not usable in this cluster: "+err.Error(),
			envSnapshotMustRun)
	}

	return &snapshotInfra{backupTestInfra: infra, dyn: dyn, class: class}
}

// eventuallyGone waits for a set of names to empty out. Deleting an object is
// asynchronous, and one held by something else stays Terminating until that
// clears, so this asserts the deletion actually finishes rather than that the
// call returned.
func eventuallyGone(t *testing.T, list func() []string, msg string) {
	t.Helper()

	var last []string

	require.Eventually(t, func() bool {
		last = list()

		return len(last) == 0
	}, 2*time.Minute, 2*time.Second, "%s; still present: %v", msg, last)
}

// listSnapshots returns the names of the VolumeSnapshots in a namespace.
func listSnapshots(t *testing.T, infra *snapshotInfra, ns string) []string {
	t.Helper()

	list, err := infra.dyn.Resource(volumeSnapshotGVR).Namespace(ns).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)

	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}

	return names
}

// listClaims returns the names of the claims in a namespace, which is how a
// leaked clone is spotted.
func listClaims(t *testing.T, infra *snapshotInfra, ns string) []string {
	t.Helper()

	list, err := infra.cli.KubeClient.CoreV1().PersistentVolumeClaims(ns).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)

	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}

	return names
}

// clonesIn returns the claims a run provisioned from a snapshot, which are the
// ones named after the release rather than by the test.
func clonesIn(t *testing.T, infra *snapshotInfra, ns string) []string {
	t.Helper()

	var clones []string

	for _, name := range listClaims(t, infra, ns) {
		if strings.HasSuffix(name, "-clone") {
			clones = append(clones, name)
		}
	}

	return clones
}

func snapshotBackup(t *testing.T, infra *snapshotInfra, ns, archiveFile string) pvmigrate.Backup {
	t.Helper()

	backup := archiveBackup(t, infra.backupTestInfra, ns, archiveFile)
	backup.Snapshot = true
	backup.SnapshotClass = infra.class

	return backup
}

func TestSnapshot(t *testing.T) {
	t.Parallel()

	infra := setupSnapshotInfra(t)

	t.Run("RoundTripLeavesNothingBehind", func(t *testing.T) {
		t.Parallel()
		testSnapshotRoundTrip(t, infra)
	})
	t.Run("MountedClaimNeedsNoIgnoreMounted", func(t *testing.T) {
		t.Parallel()
		testSnapshotMountedClaim(t, infra)
	})
	t.Run("KeepSnapshotSurvivesCleanup", func(t *testing.T) {
		t.Parallel()
		testSnapshotKeepSurvivesCleanup(t, infra)
	})
	t.Run("FromExistingSnapshot", func(t *testing.T) {
		t.Parallel()
		testSnapshotFromExisting(t, infra)
	})
	t.Run("UnknownClassFailsWithoutLeftovers", func(t *testing.T) {
		t.Parallel()
		testSnapshotUnknownClass(t, infra)
	})
	t.Run("ReadWriteOncePodSource", func(t *testing.T) {
		t.Parallel()
		testSnapshotReadWriteOncePod(t, infra)
	})
}

//nolint:thelper // subtest implementation, not a helper
func testSnapshotRoundTrip(t *testing.T, infra *snapshotInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra.backupTestInfra, place, archiveSeedCmd())
	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")
	placePod(t, infra.backupTestInfra, place, ns, "dest-pvc", "dest-pod")

	before := fingerprint(t, infra.backupTestInfra, ns, "test-pod")
	archiveFile := "archive-pvc:/snap.tar.zst"

	require.NoError(t, pvmigrate.RunBackup(t.Context(), snapshotBackup(t, infra, ns, archiveFile)))

	eventuallyGone(t, func() []string { return listSnapshots(t, infra, ns) },
		"a finished run removes the snapshot it cut")
	eventuallyGone(t, func() []string { return clonesIn(t, infra, ns) },
		"a finished run removes the clone it made")

	require.NoError(t, pvmigrate.RunRestore(t.Context(),
		archiveRestore(t, infra.backupTestInfra, ns, "dest-pvc", archiveFile)))

	assert.Equal(t, before, fingerprint(t, infra.backupTestInfra, ns, "dest-pod"))
}

// testSnapshotMountedClaim pins the point of the workflow: the live claim is
// mounted by a running pod and that is not a conflict, because the job reads a
// clone and never touches the live volume.
//
//nolint:thelper // subtest implementation, not a helper
func testSnapshotMountedClaim(t *testing.T, infra *snapshotInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra.backupTestInfra, place, archiveSeedCmd())
	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")

	backup := snapshotBackup(t, infra, ns, "archive-pvc:/snap.tar.zst")
	backup.IgnoreMounted = false

	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup),
		"a snapshot-backed backup reads a clone, so a mounted live claim is expected")
}

// testSnapshotKeepSurvivesCleanup runs detached so a release still exists when
// cleanup runs, which is what makes cleanup sweep by label at all. A snapshot
// the user asked to keep must survive that sweep while the clone does not.
//
//nolint:thelper // subtest implementation, not a helper
func testSnapshotKeepSurvivesCleanup(t *testing.T, infra *snapshotInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra.backupTestInfra, place, archiveSeedCmd())
	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")

	migrationID := "keep-" + randomSuffix()

	backup := snapshotBackup(t, infra, ns, "archive-pvc:/keep.tar.zst")
	backup.ID = migrationID
	backup.KeepSnapshot = true
	backup.Detach = true

	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup))

	// The detached job holds the clone until it finishes, and cleanup refuses
	// to run while a job is still active. The archive file appears the moment
	// tar creates it, well before the job is done, so the job's own status is
	// what to wait on.
	jobName := "pv-migrate-" + migrationID + "-backup-tar"

	require.Eventually(t, func() bool {
		job, err := infra.cli.KubeClient.BatchV1().Jobs(ns).Get(t.Context(), jobName, metav1.GetOptions{})

		return err == nil && job.Status.Succeeded > 0
	}, 5*time.Minute, 5*time.Second, "the detached backup job %s should complete", jobName)

	snapshots := listSnapshots(t, infra, ns)
	require.Len(t, snapshots, 1, "the kept snapshot should be there before cleanup")
	kept := snapshots[0]

	// pv-migrate's own cleanup, not a Helm uninstall: the clone is not part of
	// the release, so only the label sweep in cleanup removes it.
	require.NoError(t, runCliAppWithArgs(t.Context(), t, "cleanup", migrationID, "-n", ns))

	eventuallyGone(t, func() []string { return clonesIn(t, infra, ns) },
		"cleanup must remove the clone it finds by label")

	assert.Contains(t, listSnapshots(t, infra, ns), kept,
		"cleanup must not remove a snapshot the run was told to keep, which carries no instance label")
}

// testSnapshotFromExisting backs up a clone of a snapshot that already exists,
// rather than cutting a new one. The snapshot is the caller's, so the run must
// leave it alone.
//
//nolint:thelper // subtest implementation, not a helper
func testSnapshotFromExisting(t *testing.T, infra *snapshotInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra.backupTestInfra, place, archiveSeedCmd())
	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")
	placePod(t, infra.backupTestInfra, place, ns, "dest-pvc", "dest-pod")

	before := fingerprint(t, infra.backupTestInfra, ns, "test-pod")
	snapName := "preexisting-" + randomSuffix()

	createSnapshot(t, infra, ns, snapName, "test-pvc")

	archiveFile := "archive-pvc:/from-existing.tar.zst"

	backup := archiveBackup(t, infra.backupTestInfra, ns, archiveFile)
	backup.FromSnapshot = snapName

	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup))

	assert.Contains(t, listSnapshots(t, infra, ns), snapName,
		"a snapshot the run did not create is not the run's to remove")

	require.NoError(t, pvmigrate.RunRestore(t.Context(),
		archiveRestore(t, infra.backupTestInfra, ns, "dest-pvc", archiveFile)))

	assert.Equal(t, before, fingerprint(t, infra.backupTestInfra, ns, "dest-pod"))
}

// testSnapshotUnknownClass pins that a class that does not exist is caught
// before anything is created, rather than at the cut timeout. With --flush
// that timeout would be spent with the database locked.
//
//nolint:thelper // subtest implementation, not a helper
func testSnapshotUnknownClass(t *testing.T, infra *snapshotInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra.backupTestInfra, place, archiveSeedCmd())
	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")

	backup := snapshotBackup(t, infra, ns, "archive-pvc:/bad.tar.zst")
	backup.SnapshotClass = "no-such-class-" + randomSuffix()
	backup.NoCleanupOnFailure = false

	start := time.Now()
	err := pvmigrate.RunBackup(t.Context(), backup)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
	assert.Less(t, time.Since(start), time.Minute,
		"an unknown class must be caught before the cut is attempted, not at its timeout")
	assert.Empty(t, listSnapshots(t, infra, ns), "a failed run must not leave a snapshot behind")
}

// testSnapshotReadWriteOncePod covers the access mode Kubernetes recommends
// for a database. A mounted one cannot be mounted by a second pod, which the
// plain path refuses, but a snapshot-backed run never mounts it.
//
//nolint:thelper // subtest implementation, not a helper
func testSnapshotReadWriteOncePod(t *testing.T, infra *snapshotInfra) {
	place := newPlacement(t, infra.cli)
	ns := newTestNS(t, infra.cli, "pvmig-rwop")

	if err := provisionRWOPPod(t.Context(), infra.cli, ns, "rwop-pvc", "rwop-pod",
		place.node, place.class, archiveSeedCmd()); err != nil {
		skipOrFail(t, "this cluster cannot provision a ReadWriteOncePod claim: "+err.Error(), envSnapshotMustRun)
	}

	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")

	backup := snapshotBackup(t, infra, ns, "archive-pvc:/rwop.tar.zst")
	backup.PVC.Name = "rwop-pvc"
	backup.IgnoreMounted = false

	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup),
		"a mounted ReadWriteOncePod claim is a valid snapshot source, since the job mounts only the clone")
}

// createSnapshot cuts a VolumeSnapshot the test owns and waits for it to be
// usable, which is what the provisioner requires before it will clone one.
func createSnapshot(t *testing.T, infra *snapshotInfra, ns, name, claimName string) {
	t.Helper()

	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"volumeSnapshotClassName": infra.class,
			"source":                  map[string]any{"persistentVolumeClaimName": claimName},
		},
	}}

	_, err := infra.dyn.Resource(volumeSnapshotGVR).Namespace(ns).Create(t.Context(), object, metav1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		got, getErr := infra.dyn.Resource(volumeSnapshotGVR).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
		if getErr != nil {
			return false
		}

		ready, _, _ := unstructured.NestedBool(got.Object, "status", "readyToUse")

		return ready
	}, 5*time.Minute, 2*time.Second, "the snapshot the test created should become ready")
}

// provisionRWOPPod provisions with the stricter access mode, which not every
// driver supports. It differs from provisionPinnedPod only in that mode, so it
// builds on the same helper and inherits its node pinning: an unpinned pod
// would drift to another node, and on node-local storage a snapshot of its
// claim could then not be cloned where the job runs.
func provisionRWOPPod(
	ctx context.Context, cli *k8s.ClusterClient, ns, pvcName, podName, node, class, seedCmd string,
) error {
	return provisionPinnedPodWithMode(ctx, cli, ns, pvcName, podName, node, class, seedCmd,
		corev1.ReadWriteOncePod)
}
