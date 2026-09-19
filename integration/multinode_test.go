//go:build integration

package integration

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/utkuozdemir/pv-migrate/internal/k8s"
	"github.com/utkuozdemir/pv-migrate/internal/pvc"
	"github.com/utkuozdemir/pv-migrate/pvmigrate"
)

// envMultiNodeMustRun turns the skip below into a failure, for a cluster where
// more than one node is expected.
const envMultiNodeMustRun = "PVMIG_TEST_MULTINODE_MUST_RUN"

// schedulableNodes returns the nodes a pod can actually land on, which is what
// decides whether this suite has anything to test.
func schedulableNodes(t *testing.T, cli *k8s.ClusterClient) []string {
	t.Helper()

	list, err := cli.KubeClient.CoreV1().Nodes().List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)

	var names []string

	for i := range list.Items {
		node := &list.Items[i]
		if node.Spec.Unschedulable {
			continue
		}

		names = append(names, node.Name)
	}

	return names
}

// TestMultiNode covers the one branch of the archive workflow's scheduling
// check that a single-node cluster cannot reach: two ReadWriteOnce claims
// already mounted on different nodes, which no single pod can mount together.
//
// The check exists so that case fails immediately with an explanation, instead
// of leaving a pod Pending until the Helm install times out.
func TestMultiNode(t *testing.T) {
	t.Parallel()

	infra := setupBackupInfra(t)

	nodes := schedulableNodes(t, infra.cli)
	if len(nodes) < 2 {
		skipOrFail(t, fmt.Sprintf("this cluster has %d schedulable node(s), and pinning two claims to "+
			"different nodes needs at least 2", len(nodes)), envMultiNodeMustRun)
	}

	t.Run("ClaimsOnDifferentNodesAreRefused", func(t *testing.T) {
		t.Parallel()
		testClaimsOnDifferentNodes(t, infra, nodes)
	})
	t.Run("ClaimsOnTheSameNodeWork", func(t *testing.T) {
		t.Parallel()
		testClaimsOnTheSameNode(t, infra, nodes)
	})
	t.Run("SnapshotCloneHonoursItsVolumeTopology", func(t *testing.T) {
		t.Parallel()
		testSnapshotCloneTopology(t, infra, nodes)
	})
}

// testClaimsOnDifferentNodes is the refusal. Both claims are ReadWriteOnce and
// each is pinned to a different node by the pod holding it, so one pod cannot
// mount both and the run must say so rather than hang.
//
//nolint:thelper // subtest implementation, not a helper
func testClaimsOnDifferentNodes(t *testing.T, infra *backupTestInfra, nodes []string) {
	class := waitForFirstConsumerClass(t, infra.cli)
	ns := newTestNS(t, infra.cli, "pvmig-multi")

	require.NoError(t, provisionPinnedPod(t.Context(), infra.cli, ns,
		"test-pvc", "test-pod", nodes[0], class, archiveSeedCmd()))
	require.NoError(t, provisionPinnedPod(t.Context(), infra.cli, ns,
		"archive-pvc", "archive-pod", nodes[1], class, ""))

	backup := archiveBackup(t, infra, ns, "archive-pvc:/db.tar.zst")
	backup.NoCleanupOnFailure = false

	err := pvmigrate.RunBackup(t.Context(), backup)

	require.Error(t, err, "two mounted ReadWriteOnce claims on different nodes cannot share a pod")
	assert.Contains(t, err.Error(), "one pod cannot mount both")
	assert.Contains(t, err.Error(), nodes[0], "the error should name the nodes involved")
	assert.Contains(t, err.Error(), nodes[1])
}

// testClaimsOnTheSameNode is the other half, and the reason the refusal above
// is not simply "two mounted claims". Co-located claims are fine, because
// ReadWriteOnce binds a volume to a node rather than to a pod.
//
//nolint:thelper // subtest implementation, not a helper
func testClaimsOnTheSameNode(t *testing.T, infra *backupTestInfra, nodes []string) {
	class := waitForFirstConsumerClass(t, infra.cli)
	ns := newTestNS(t, infra.cli, "pvmig-multi")

	require.NoError(t, provisionPinnedPod(t.Context(), infra.cli, ns,
		"test-pvc", "test-pod", nodes[0], class, archiveSeedCmd()))
	require.NoError(t, provisionPinnedPod(t.Context(), infra.cli, ns,
		"archive-pvc", "archive-pod", nodes[0], class, ""))
	require.NoError(t, provisionPinnedPod(t.Context(), infra.cli, ns,
		"dest-pvc", "dest-pod", nodes[0], class, ""))

	before := fingerprint(t, infra, ns, "test-pod")
	archiveFile := "archive-pvc:/db.tar.zst"

	require.NoError(t, pvmigrate.RunBackup(t.Context(), archiveBackup(t, infra, ns, archiveFile)))
	require.NoError(t, pvmigrate.RunRestore(t.Context(), archiveRestore(t, infra, ns, "dest-pvc", archiveFile)))

	assert.Equal(t, before, fingerprint(t, infra, ns, "dest-pod"))
}

// requireNodeLocalVolume skips unless a claim's volume lives on exactly one
// node, which the refusal below depends on.
//
// A replicated or network-attached volume is reachable from several nodes, so
// a clone of it intersects with the archive claim's node and one pod really
// can mount both. The run then succeeds, and that is the right answer rather
// than a failure: the clone still inherited its source volume's topology,
// that topology is simply not restrictive. Only node-local storage can
// produce the refusal.
func requireNodeLocalVolume(t *testing.T, infra *backupTestInfra, ns, claimName, class string) {
	t.Helper()

	claim, err := infra.cli.KubeClient.CoreV1().PersistentVolumeClaims(ns).
		Get(t.Context(), claimName, metav1.GetOptions{})
	require.NoError(t, err)

	allowed, err := pvc.AllowedNodesFor(t.Context(), infra.cli.KubeClient, claim)
	require.NoError(t, err)

	if len(allowed) == 1 {
		return
	}

	where := fmt.Sprintf("%d nodes", len(allowed))
	if allowed == nil {
		where = "any node"
	}

	t.Skipf("storage class %s puts a volume on %s, and this case needs a volume "+
		"bound to one node; nothing here is wrong, the refusal it asserts "+
		"cannot arise on storage every node can reach", class, where)
}

// testSnapshotCloneTopology is the case a "which pod has this mounted" check
// cannot see. The backup reads a clone of a snapshot, and that clone has never
// been mounted by anything, yet its volume is pinned to the node its snapshot
// lives on. With the archive claim pinned to the other node, no pod can mount
// both, and the run has to say so rather than leave a pod Pending until the
// install times out.
//
//nolint:thelper // subtest implementation, not a helper
func testSnapshotCloneTopology(t *testing.T, infra *backupTestInfra, nodes []string) {
	class := waitForFirstConsumerClass(t, infra.cli)

	snapClass := os.Getenv(envSnapshotClass)
	if snapClass == "" {
		skipOrFail(t, envSnapshotClass+" not set, so no snapshot can be cut", envMultiNodeMustRun)
	}

	ns := newTestNS(t, infra.cli, "pvmig-multi")

	// The source and the archive land on different nodes, so the clone taken
	// from the source is pinned away from the archive claim.
	require.NoError(t, provisionPinnedPod(t.Context(), infra.cli, ns,
		"test-pvc", "test-pod", nodes[0], class, archiveSeedCmd()))
	require.NoError(t, provisionPinnedPod(t.Context(), infra.cli, ns,
		"archive-pvc", "archive-pod", nodes[1], class, ""))

	requireNodeLocalVolume(t, infra, ns, "test-pvc", class)

	backup := archiveBackup(t, infra, ns, "archive-pvc:/db.tar.zst")
	backup.Snapshot = true
	backup.SnapshotClass = snapClass
	backup.IgnoreMounted = false
	backup.NoCleanupOnFailure = false

	err := pvmigrate.RunBackup(t.Context(), backup)

	require.Error(t, err, "a clone pinned to one node cannot be read beside an archive claim on another")
	assert.Contains(t, err.Error(), "one pod cannot mount both")
	assert.NotContains(t, err.Error(), "timed out",
		"this has to be refused up front, not discovered when the pod fails to schedule")
}
