package snapshot_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/utkuozdemir/pv-migrate/internal/snapshot"
)

var (
	snapshotGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshots",
	}
	contentGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshotcontents",
	}
	classGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshotclasses",
	}
)

func newClient(
	t *testing.T,
	objects ...runtime.Object,
) (*snapshot.Client, *dynamicfake.FakeDynamicClient, *kubefake.Clientset) {
	t.Helper()

	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		snapshotGVR: "VolumeSnapshotList",
		contentGVR:  "VolumeSnapshotContentList",
		classGVR:    "VolumeSnapshotClassList",
	}

	var dynObjects, kubeObjects []runtime.Object

	for _, object := range objects {
		if _, ok := object.(*unstructured.Unstructured); ok {
			dynObjects = append(dynObjects, object)
		} else {
			kubeObjects = append(kubeObjects, object)
		}
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, dynObjects...)
	kube := kubefake.NewSimpleClientset(kubeObjects...)

	// Discovery is how a missing class is told apart from a missing API, so a
	// cluster that has the API has to say so here.
	kube.Resources = []*metav1.APIResourceList{{
		GroupVersion: "snapshot.storage.k8s.io/v1",
		APIResources: []metav1.APIResource{
			{Name: "volumesnapshots", Namespaced: true, Kind: "VolumeSnapshot"},
			{Name: "volumesnapshotclasses", Kind: "VolumeSnapshotClass"},
		},
	}}

	return snapshot.NewWithClients(dyn, kube), dyn, kube
}

func vs(name, contentName string, ready bool, errMsg string) *unstructured.Unstructured {
	status := map[string]any{"readyToUse": ready}
	if contentName != "" {
		status["boundVolumeSnapshotContentName"] = contentName
	}

	if errMsg != "" {
		status["error"] = map[string]any{"message": errMsg}
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]any{"name": name, "namespace": "ns"},
		"status":     status,
	}}
}

func vsc(name, handle string) *unstructured.Unstructured {
	status := map[string]any{}
	if handle != "" {
		status["snapshotHandle"] = handle
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshotContent",
		"metadata":   map[string]any{"name": name},
		"status":     status,
	}}
}

func TestCreate_SetsSourceClassAndLabels(t *testing.T) {
	t.Parallel()

	client, dyn, _ := newClient(t)

	err := client.Create(context.Background(), "ns", "mysql-data", "csi-class", "snap-1",
		map[string]string{"app.kubernetes.io/instance": "rel"})
	require.NoError(t, err)

	got, err := dyn.Resource(snapshotGVR).Namespace("ns").Get(context.Background(), "snap-1", metav1.GetOptions{})
	require.NoError(t, err)

	claim, _, _ := unstructured.NestedString(got.Object, "spec", "source", "persistentVolumeClaimName")
	class, _, _ := unstructured.NestedString(got.Object, "spec", "volumeSnapshotClassName")

	assert.Equal(t, "mysql-data", claim)
	assert.Equal(t, "csi-class", class)
	assert.Equal(t, "rel", got.GetLabels()["app.kubernetes.io/instance"])
}

func TestCreate_OmitsClassWhenUnset(t *testing.T) {
	t.Parallel()

	client, dyn, _ := newClient(t)
	require.NoError(t, client.Create(context.Background(), "ns", "pvc", "", "snap-1", nil))

	got, err := dyn.Resource(snapshotGVR).Namespace("ns").Get(context.Background(), "snap-1", metav1.GetOptions{})
	require.NoError(t, err)

	_, found, _ := unstructured.NestedString(got.Object, "spec", "volumeSnapshotClassName")
	assert.False(t, found, "an unset class must be left to the cluster default, not sent as an empty string")
}

func TestWaitForCut_ReturnsOnceHandleExists(t *testing.T) {
	t.Parallel()

	client, _, _ := newClient(t, vs("snap-1", "content-1", false, ""), vsc("content-1", "handle-abc"))

	err := client.WaitForCut(context.Background(), "ns", "snap-1", 5*time.Second, slog.Default())
	require.NoError(t, err)
}

// The cut is the handle, not readyToUse. A snapshot that is cut but not yet
// ready must not keep a database lock waiting.
func TestWaitForCut_DoesNotWaitForReady(t *testing.T) {
	t.Parallel()

	client, _, _ := newClient(t, vs("snap-1", "content-1", false, ""), vsc("content-1", "handle-abc"))

	require.NoError(t, client.WaitForCut(context.Background(), "ns", "snap-1", 5*time.Second, nil))
}

func TestWaitForCut_TimesOutWithoutHandle(t *testing.T) {
	t.Parallel()

	client, _, _ := newClient(t, vs("snap-1", "content-1", false, ""), vsc("content-1", ""))

	err := client.WaitForCut(context.Background(), "ns", "snap-1", 300*time.Millisecond, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Contains(t, err.Error(), "the snapshot to be cut")
}

// A status.error is the controller's last observed error, which it retries
// past and clears on success, so the wait goes on. The message is kept for the
// timeout, where it is the most useful thing to say.
func TestWaitForCut_KeepsWaitingThroughAnErrorAndReportsItOnTimeout(t *testing.T) {
	t.Parallel()

	client, _, _ := newClient(t, vs("snap-1", "", false, "driver said no"))

	err := client.WaitForCut(context.Background(), "ns", "snap-1", 300*time.Millisecond, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Contains(t, err.Error(), "driver said no")
}

func TestWaitReady(t *testing.T) {
	t.Parallel()

	t.Run("ready", func(t *testing.T) {
		t.Parallel()

		client, _, _ := newClient(t, vs("snap-1", "content-1", true, ""))
		require.NoError(t, client.WaitReady(context.Background(), "ns", "snap-1", 5*time.Second))
	})

	t.Run("not ready times out", func(t *testing.T) {
		t.Parallel()

		client, _, _ := newClient(t, vs("snap-1", "content-1", false, ""))

		err := client.WaitReady(context.Background(), "ns", "snap-1", 300*time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ready to use")
	})

	t.Run("error status is reported on timeout, not before", func(t *testing.T) {
		t.Parallel()

		client, _, _ := newClient(t, vs("snap-1", "", false, "quota exceeded"))

		err := client.WaitReady(context.Background(), "ns", "snap-1", 300*time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
		assert.Contains(t, err.Error(), "quota exceeded")
	})

	t.Run("a cleared error does not linger", func(t *testing.T) {
		t.Parallel()

		client, _, _ := newClient(t, vs("snap-1", "content-1", true, ""))
		require.NoError(t, client.WaitReady(context.Background(), "ns", "snap-1", 5*time.Second))
	})
}

func sourceClaim(requested, capacity string) *corev1.PersistentVolumeClaim {
	class := "fast"
	claim := &corev1.PersistentVolumeClaim{
		Name: "mysql-data", Namespace: "ns",
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(requested)},
			},
		},
	}

	if capacity != "" {
		claim.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)}
	}

	return claim
}

func TestClone_CopiesClassModesAndDataSource(t *testing.T) {
	t.Parallel()

	client, _, kube := newClient(t)
	source := sourceClaim("10Gi", "")

	require.NoError(t, client.Clone(context.Background(), "snap-1", "clone-1", source,
		map[string]string{"app.kubernetes.io/instance": "rel"}))

	got, err := kube.CoreV1().PersistentVolumeClaims("ns").Get(context.Background(), "clone-1", metav1.GetOptions{})
	require.NoError(t, err)

	assert.Equal(t, "fast", *got.Spec.StorageClassName)
	assert.Equal(t, source.Spec.AccessModes, got.Spec.AccessModes)
	assert.Equal(t, "10Gi", got.Spec.Resources.Requests.Storage().String())
	assert.Equal(t, "rel", got.Labels["app.kubernetes.io/instance"])

	require.NotNil(t, got.Spec.DataSource)
	assert.Equal(t, "VolumeSnapshot", got.Spec.DataSource.Kind)
	assert.Equal(t, "snap-1", got.Spec.DataSource.Name)
	assert.Equal(t, "snapshot.storage.k8s.io", *got.Spec.DataSource.APIGroup)
}

// A bound claim can be larger than it asked for, and a restore is refused
// below the snapshot's size, so the clone asks for the bound capacity.
func TestClone_RequestsBoundCapacityWhenLarger(t *testing.T) {
	t.Parallel()

	client, _, kube := newClient(t)

	require.NoError(t, client.Clone(context.Background(), "snap-1", "clone-1", sourceClaim("10Gi", "12Gi"), nil))

	got, err := kube.CoreV1().PersistentVolumeClaims("ns").Get(context.Background(), "clone-1", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "12Gi", got.Spec.Resources.Requests.Storage().String())
}

func TestDelete_ToleratesMissing(t *testing.T) {
	t.Parallel()

	client, _, _ := newClient(t)

	require.NoError(t, client.DeleteSnapshot(context.Background(), "ns", "gone"))
	require.NoError(t, client.DeleteClaim(context.Background(), "ns", "gone"))
}

func TestDeleteLabeled_RemovesClaimsThenSnapshots(t *testing.T) {
	t.Parallel()

	labeled := vs("snap-1", "content-1", true, "")
	labeled.SetLabels(map[string]string{"app.kubernetes.io/instance": "rel"})

	other := vs("snap-other", "", true, "")

	clone := sourceClaim("1Gi", "")
	clone.Name = "clone-1"
	clone.Labels = map[string]string{"app.kubernetes.io/instance": "rel"}
	keep := sourceClaim("1Gi", "")
	keep.Name = "keep-me"

	client, dyn, kube := newClient(t, labeled, other, clone, keep)

	removed, err := client.DeleteLabeled(context.Background(), "ns", "app.kubernetes.io/instance=rel")
	require.NoError(t, err)
	assert.Equal(t, []string{"claim clone-1", "snapshot snap-1"}, removed)

	_, err = kube.CoreV1().PersistentVolumeClaims("ns").Get(context.Background(), "keep-me", metav1.GetOptions{})
	require.NoError(t, err, "an unlabeled claim must survive")

	_, err = dyn.Resource(snapshotGVR).Namespace("ns").Get(context.Background(), "snap-other", metav1.GetOptions{})
	require.NoError(t, err, "an unlabeled snapshot must survive")
}

func snapClass(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshotClass",
		"metadata":   map[string]any{"name": name},
	}}
}

// The class is checked before a database is quiesced, so a misspelled one
// costs a round trip rather than the whole cut timeout with the lock held.
func TestCheckClass(t *testing.T) {
	t.Parallel()

	client, _, _ := newClient(t, snapClass("csi-hostpath-snapclass"))

	require.NoError(t, client.CheckClass(context.Background(), "csi-hostpath-snapclass"))
	require.NoError(t, client.CheckClass(context.Background(), ""), "empty means the cluster default")

	err := client.CheckClass(context.Background(), "no-such-class")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

// A cluster with no snapshot API answers 404 for a class exactly as a cluster
// that simply lacks that class does, so the two have to be told apart by
// discovery rather than by the status code.
func TestCheckClass_ApiNotInstalled(t *testing.T) {
	t.Parallel()

	client, _, kube := newClient(t)
	kube.Resources = nil

	err := client.CheckClass(context.Background(), "csi-hostpath-snapclass")
	require.Error(t, err)
	require.ErrorIs(t, err, snapshot.ErrNotInstalled)
	assert.NotContains(t, err.Error(), "does not exist",
		"a cluster without the API should not be described as missing one class")
}
