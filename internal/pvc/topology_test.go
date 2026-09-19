package pvc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/utkuozdemir/pv-migrate/internal/pvc"
)

// node returns a node carrying the labels a CSI driver publishes topology
// against: its own name, and the zone it sits in.
func node(name, zone string) *corev1.Node {
	return &corev1.Node{
		Name: name,
		Labels: map[string]string{
			corev1.LabelHostname:         name,
			corev1.LabelTopologyZone:     zone,
			"topology.hostpath.csi/node": name,
		},
	}
}

// boundClaim returns a claim bound to a volume, with that volume's node
// affinity expressed over the given label and values.
func boundClaim(
	name, key string,
	values ...string,
) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	const volumeName = "pv-1"

	claim := &corev1.PersistentVolumeClaim{
		Namespace: "ns", Name: name,
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: volumeName},
	}

	volume := &corev1.PersistentVolume{Name: volumeName}

	if key != "" {
		volume.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{
			Required: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      key,
						Operator: corev1.NodeSelectorOpIn,
						Values:   values,
					}},
				}},
			},
		}
	}

	return claim, volume
}

// A node-local volume reports one node, and it does so whether or not anything
// has it mounted. This is the case a "which pod has it mounted" check misses
// entirely: a claim cloned from a snapshot has never been mounted.
func TestAllowedNodesFor_NodeLocalVolume(t *testing.T) {
	t.Parallel()

	claim, volume := boundClaim("clone", "topology.hostpath.csi/node", "node-b")
	kube := kubefake.NewSimpleClientset(claim, volume, node("node-a", "zone-1"), node("node-b", "zone-2"))

	allowed, err := pvc.AllowedNodesFor(context.Background(), kube, claim)
	require.NoError(t, err)
	assert.Equal(t, []string{"node-b"}, allowed)
}

// A cloud disk is pinned to its availability zone rather than to a node, so it
// resolves to every node in that zone.
func TestAllowedNodesFor_ZonalVolume(t *testing.T) {
	t.Parallel()

	claim, volume := boundClaim("data", corev1.LabelTopologyZone, "zone-1")
	kube := kubefake.NewSimpleClientset(claim, volume,
		node("node-a", "zone-1"), node("node-b", "zone-1"), node("node-c", "zone-2"))

	allowed, err := pvc.AllowedNodesFor(context.Background(), kube, claim)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"node-a", "node-b"}, allowed)
	assert.NotContains(t, allowed, "node-c", "a volume in one zone cannot be used in another")
}

func TestAllowedNodesFor_Unconstrained(t *testing.T) {
	t.Parallel()

	claim, volume := boundClaim("data", "")
	kube := kubefake.NewSimpleClientset(claim, volume, node("node-a", "zone-1"))

	allowed, err := pvc.AllowedNodesFor(context.Background(), kube, claim)
	require.NoError(t, err)
	assert.Nil(t, allowed, "storage reachable from anywhere constrains nothing")
}

func TestAllowedNodesFor_UnboundClaim(t *testing.T) {
	t.Parallel()

	claim := &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "pending"}
	kube := kubefake.NewSimpleClientset(claim, node("node-a", "zone-1"))

	allowed, err := pvc.AllowedNodesFor(context.Background(), kube, claim)
	require.NoError(t, err)
	assert.Nil(t, allowed, "a claim with no volume yet says nothing about topology")
}

func TestIntersectNodes(t *testing.T) {
	t.Parallel()

	assert.Nil(t, pvc.IntersectNodes(nil, nil))
	assert.Equal(t, []string{"a"}, pvc.IntersectNodes(nil, []string{"a"}))
	assert.Equal(t, []string{"a"}, pvc.IntersectNodes([]string{"a"}, nil))
	assert.Equal(t, []string{"b"}, pvc.IntersectNodes([]string{"a", "b"}, []string{"b", "c"}))

	// Empty is a real answer and must not read as unconstrained: it means
	// nothing satisfies both.
	both := pvc.IntersectNodes([]string{"a"}, []string{"b"})
	assert.NotNil(t, both)
	assert.Empty(t, both)
}

func TestRequireNodes(t *testing.T) {
	t.Parallel()

	assert.Nil(t, pvc.RequireNodes(nil), "nil means unconstrained")

	// An empty set is not unconstrained, it is a constraint nothing satisfies,
	// and emitting no affinity for it would let a job schedule anywhere.
	assert.NotNil(t, pvc.RequireNodes([]string{}))

	got := pvc.RequireNodes([]string{"node-a"})
	require.NotNil(t, got)

	affinity, ok := got["nodeAffinity"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, affinity, "requiredDuringSchedulingIgnoredDuringExecution",
		"a volume's topology is a hard limit, not a preference")
}

// Volumes and nodes are cluster-scoped, and the namespace-scoped account the
// docs describe cannot read them. Losing the topology has to cost the early
// conflict check and nothing else, or this breaks every account that worked
// before the topology was read at all.
func TestAllowedNodesFor_ForbiddenDegrades(t *testing.T) {
	t.Parallel()

	claim, volume := boundClaim("data", "topology.hostpath.csi/node", "node-a")

	for _, forbidden := range []string{"persistentvolumes", "nodes"} {
		t.Run(forbidden, func(t *testing.T) {
			t.Parallel()

			kube := kubefake.NewSimpleClientset(claim, volume, node("node-a", "zone-1"))
			kube.PrependReactor("get", forbidden, denyAccess(forbidden))
			kube.PrependReactor("list", forbidden, denyAccess(forbidden))

			allowed, err := pvc.AllowedNodesFor(context.Background(), kube, claim)
			require.NoError(t, err, "a refused read must not fail the operation")
			assert.Nil(t, allowed, "unknown topology constrains nothing")
		})
	}
}

// denyAccess makes the fake answer a read the way an API server answers one
// the caller has no permission for.
func denyAccess(resource string) k8stesting.ReactionFunc {
	return func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: resource}, "", errors.New("not allowed"))
	}
}
