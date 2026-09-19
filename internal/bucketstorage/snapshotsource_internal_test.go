package bucketstorage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/utkuozdemir/pv-migrate/internal/pvc"
	"github.com/utkuozdemir/pv-migrate/internal/rclone"
)

func TestUsesSnapshot(t *testing.T) {
	t.Parallel()

	assert.False(t, usesSnapshot(&Request{}))
	assert.True(t, usesSnapshot(&Request{Snapshot: true}))
	assert.True(t, usesSnapshot(&Request{FromSnapshot: "existing"}))
	assert.True(t, usesSnapshot(&Request{Flush: "mysql"}), "--flush implies a snapshot")
}

func TestValidateSnapshotRequest_OK(t *testing.T) {
	t.Parallel()

	for name, req := range map[string]Request{
		"plain run":         {Direction: rclone.DirectionBackup},
		"snapshot":          {Direction: rclone.DirectionBackup, Snapshot: true},
		"from snapshot":     {Direction: rclone.DirectionBackup, FromSnapshot: "snap"},
		"flush":             {Direction: rclone.DirectionBackup, Flush: "mysql"},
		"restore, no flags": {Direction: rclone.DirectionRestore},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, validateSnapshotRequest(&req))
		})
	}
}

func TestValidateSnapshotRequest_Errors(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		req     Request
		wantErr string
	}{
		"snapshot on restore": {
			req:     Request{Direction: rclone.DirectionRestore, Snapshot: true},
			wantErr: "apply to a backup, not a restore",
		},
		"flush on restore": {
			req:     Request{Direction: rclone.DirectionRestore, Flush: "mysql"},
			wantErr: "apply to a backup, not a restore",
		},
		"flush with from-snapshot": {
			req:     Request{Direction: rclone.DirectionBackup, Flush: "mysql", FromSnapshot: "snap"},
			wantErr: "cannot be combined with --from-snapshot",
		},
		"unknown flush kind": {
			req:     Request{Direction: rclone.DirectionBackup, Flush: "db2"},
			wantErr: `unsupported --flush "db2"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := validateSnapshotRequest(&tc.req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// A kept snapshot must not carry the instance label, or the cleanup that
// removes the run's other leftovers takes it too.
func TestSnapshotLabels(t *testing.T) {
	t.Parallel()

	kept := snapshotLabels("rel", true)
	assert.NotContains(t, kept, "app.kubernetes.io/instance")
	assert.Equal(t, "pv-migrate", kept["app.kubernetes.io/name"])

	removable := snapshotLabels("rel", false)
	assert.Equal(t, "rel", removable["app.kubernetes.io/instance"])
	assert.Equal(t, runLabels("rel"), removable)
}

func TestPrepareFlush_RefusesCommandOverrideForPairKinds(t *testing.T) {
	t.Parallel()

	source := &pvc.Info{
		Claim:      &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "data"},
		MountedPod: "mongo-0",
	}

	_, _, err := prepareFlush(nil, &Request{Flush: "mongodb", FlushCommand: []string{"mongosh"}}, source)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runs three")

	spec, _, err := prepareFlush(
		nil,
		&Request{Flush: "postgres", FlushCommand: []string{"psql", "-c", "CHECKPOINT"}},
		source,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"psql", "-c", "CHECKPOINT"}, spec.Command)
}

func TestPrepareFlush_NeedsAMountedPod(t *testing.T) {
	t.Parallel()

	source := &pvc.Info{Claim: &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "data"}}

	_, _, err := prepareFlush(nil, &Request{Flush: "mysql"}, source)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pod has claim data mounted")
}

// A clone bound on first consumer has no volume yet, so it reports no
// topology of its own. Inheriting the source's is what keeps a snapshot from
// looking restorable on a node its storage can never reach.
func TestInheritTopology(t *testing.T) {
	t.Parallel()

	source := &pvc.Info{
		Claim:        &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "data"},
		AllowedNodes: []string{"node-a"},
		PinnedNodes:  []string{"node-a"},
	}

	t.Run("an unbound clone takes the source's", func(t *testing.T) {
		t.Parallel()

		clone := &pvc.Info{Claim: &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "clone"}}
		inheritTopology(clone, source)

		assert.Equal(t, []string{"node-a"}, clone.PinnedNodes)
		assert.Equal(t, pvc.RequireNodes([]string{"node-a"}), clone.AffinityHelmValues)
	})

	t.Run("a bound clone keeps its own", func(t *testing.T) {
		t.Parallel()

		clone := &pvc.Info{
			Claim:        &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "clone"},
			AllowedNodes: []string{"node-b"},
			PinnedNodes:  []string{"node-b"},
		}
		inheritTopology(clone, source)

		assert.Equal(t, []string{"node-b"}, clone.PinnedNodes,
			"what the driver actually published beats anything inferred")
	})

	t.Run("an unconstrained source constrains nothing", func(t *testing.T) {
		t.Parallel()

		clone := &pvc.Info{Claim: &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "clone"}}
		inheritTopology(clone, &pvc.Info{Claim: source.Claim})

		assert.Nil(t, clone.PinnedNodes)
	})
}
