package pvc_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/utkuozdemir/pv-migrate/internal/k8s"
	"github.com/utkuozdemir/pv-migrate/internal/pvc"
)

func rwopClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Namespace: "ns",
		Name:      "data",
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
		},
	}
}

func podMounting(name, node string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		Namespace: "ns", Name: name,
		Spec: corev1.PodSpec{
			NodeName: node,
			Volumes: []corev1.Volume{{
				Name:                  "v",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"},
			}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func clusterClient(objects ...runtime.Object) *k8s.ClusterClient {
	return &k8s.ClusterClient{KubeClient: kubefake.NewSimpleClientset(objects...)}
}

// A mounted ReadWriteOncePod claim cannot be mounted by a second pod, which
// New refuses. A snapshot source is never mounted, so NewSource must not.
func TestNewSource_AcceptsMountedRWOP(t *testing.T) {
	t.Parallel()

	client := clusterClient(rwopClaim(), podMounting("db-0", "node-a", corev1.PodRunning))

	_, err := pvc.New(context.Background(), client, "ns", "data")
	require.Error(t, err, "New must still refuse it")
	assert.Contains(t, err.Error(), "ReadWriteOncePod")

	info, err := pvc.NewSource(context.Background(), client, "ns", "data")
	require.NoError(t, err)
	assert.Equal(t, "db-0", info.MountedPod)
	assert.Equal(t, "node-a", info.MountedNode)
}

// A pod that has finished still lists its volumes but holds nothing, and a
// flush aimed at it would talk to a database that is not running.
func TestMountedPod_SkipsFinishedPods(t *testing.T) {
	t.Parallel()

	client := clusterClient(rwopClaim(),
		podMounting("old-job", "node-x", corev1.PodSucceeded),
		podMounting("crashed", "node-y", corev1.PodFailed),
		podMounting("db-0", "node-a", corev1.PodRunning),
	)

	info, err := pvc.NewSource(context.Background(), client, "ns", "data")
	require.NoError(t, err)
	assert.Equal(t, "db-0", info.MountedPod)
	assert.Equal(t, "node-a", info.MountedNode)
}

func TestMountedPod_OnlyFinishedPodsMeansUnmounted(t *testing.T) {
	t.Parallel()

	client := clusterClient(rwopClaim(), podMounting("old-job", "node-x", corev1.PodSucceeded))

	info, err := pvc.New(context.Background(), client, "ns", "data")
	require.NoError(t, err)
	assert.Empty(t, info.MountedPod)
	assert.Empty(t, info.MountedNode)
}
