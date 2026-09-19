// Package snapshot creates a VolumeSnapshot of a claim and clones a claim from
// it, which is how a backup reads a point-in-time copy instead of the live
// volume.
//
// The snapshot API is a set of CRDs rather than a core type, so the objects are
// handled as unstructured values through the dynamic client. That keeps the
// typed snapshotter client out of the module and works against any cluster
// that has the CRDs installed.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	group    = "snapshot.storage.k8s.io"
	version1 = "v1"

	// pollInterval is how often the snapshot's status is read while waiting.
	pollInterval = 2 * time.Second
)

var (
	snapshotGVR = schema.GroupVersionResource{Group: group, Version: version1, Resource: "volumesnapshots"}
	contentGVR  = schema.GroupVersionResource{Group: group, Version: version1, Resource: "volumesnapshotcontents"}
	classGVR    = schema.GroupVersionResource{Group: group, Version: version1, Resource: "volumesnapshotclasses"}

	// ErrNotInstalled reports a cluster without the snapshot CRDs, which the
	// Kubernetes version alone does not guarantee.
	ErrNotInstalled = errors.New("the VolumeSnapshot API is not installed in this cluster")
)

// Client talks to the snapshot CRDs and to core claims.
type Client struct {
	dyn  dynamic.Interface
	kube kubernetes.Interface
}

// New builds a Client from the cluster's REST config.
func New(restConfig *rest.Config, kube kubernetes.Interface) (*Client, error) {
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	return NewWithClients(dyn, kube), nil
}

// NewWithClients builds a Client from ready-made clients, which is how a test
// hands in fakes.
func NewWithClients(dyn dynamic.Interface, kube kubernetes.Interface) *Client {
	return &Client{dyn: dyn, kube: kube}
}

// CheckClass reports whether the named VolumeSnapshotClass exists. An empty
// name is the cluster default, which cannot be checked this way and is left to
// the snapshot controller.
func (c *Client) CheckClass(ctx context.Context, className string) error {
	if className == "" {
		return nil
	}

	_, err := c.dyn.Resource(classGVR).Get(ctx, className, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get VolumeSnapshotClass %s: %w", className, err)
		}

		// A missing class and a missing API both answer 404, so the status
		// alone cannot tell them apart. Discovery can: it says whether the
		// group version is served at all.
		if !c.apiInstalled() {
			return ErrNotInstalled
		}

		return fmt.Errorf("VolumeSnapshotClass %s does not exist", className)
	}

	return nil
}

// Create makes a VolumeSnapshot of the named claim. The labels let a later
// cleanup find it by the same instance label the chart puts on everything else.
func (c *Client) Create(
	ctx context.Context, namespace, claimName, className, snapshotName string, labels map[string]string,
) error {
	spec := map[string]any{
		"source": map[string]any{"persistentVolumeClaimName": claimName},
	}

	if className != "" {
		spec["volumeSnapshotClassName"] = className
	}

	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/" + version1,
		"kind":       "VolumeSnapshot",
		"metadata": map[string]any{
			"name":      snapshotName,
			"namespace": namespace,
			"labels":    toAny(labels),
		},
		"spec": spec,
	}}

	_, err := c.dyn.Resource(snapshotGVR).Namespace(namespace).Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && !c.apiInstalled() {
			return ErrNotInstalled
		}

		return fmt.Errorf("failed to create VolumeSnapshot %s/%s: %w", namespace, snapshotName, err)
	}

	return nil
}

// WaitForCut blocks until the storage system has cut the snapshot, which is the
// moment its VolumeSnapshotContent carries a snapshotHandle. That is the point a
// database lock can be released: the data is fixed from here on, however long
// the driver takes to finish whatever it does afterwards.
//
// It deliberately does not wait for readyToUse. On drivers that post-process a
// snapshot asynchronously, readyToUse can lag the cut by a long time, and a
// lock held across that would stall the database for the same duration.
func (c *Client) WaitForCut(
	ctx context.Context, namespace, snapshotName string, timeout time.Duration, logger *slog.Logger,
) error {
	var lastError string

	return c.poll(ctx, timeout, func(ctx context.Context) (bool, error) {
		contentName, err := c.boundContentName(ctx, namespace, snapshotName, &lastError)
		if err != nil || contentName == "" {
			return false, err
		}

		content, err := c.dyn.Resource(contentGVR).Get(ctx, contentName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to get VolumeSnapshotContent %s: %w", contentName, err)
		}

		handle, _, _ := unstructured.NestedString(content.Object, "status", "snapshotHandle")
		if handle == "" {
			return false, nil
		}

		if logger != nil {
			logger.Info(fmt.Sprintf("📸 snapshot %s cut, handle %s", snapshotName, handle))
		}

		return true, nil
	}, "the snapshot to be cut", &lastError)
}

// WaitReady blocks until the snapshot reports readyToUse, which the CSI
// provisioner requires before it will provision a claim from it.
func (c *Client) WaitReady(
	ctx context.Context, namespace, snapshotName string, timeout time.Duration,
) error {
	var lastError string

	return c.poll(ctx, timeout, func(ctx context.Context) (bool, error) {
		snapshot, err := c.dyn.Resource(snapshotGVR).Namespace(namespace).Get(ctx, snapshotName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to get VolumeSnapshot %s/%s: %w", namespace, snapshotName, err)
		}

		noteError(snapshot, &lastError)

		ready, _, _ := unstructured.NestedBool(snapshot.Object, "status", "readyToUse")

		return ready, nil
	}, "the snapshot to become ready to use", &lastError)
}

// noteError records the snapshot's status.error, which is the last error the
// controller saw and not a verdict: it keeps retrying past one and clears it
// on success. So the wait goes on, and the message is kept for the timeout
// error, where it is the most useful thing to say.
func noteError(snapshot *unstructured.Unstructured, lastError *string) {
	if message, found, _ := unstructured.NestedString(snapshot.Object, "status", "error", "message"); found {
		*lastError = message
	}
}

// Clone creates a claim provisioned from the snapshot, in the same namespace,
// with the source claim's storage class, access modes and size. Those are the
// constraints the API puts on a snapshot restore, and a restore always makes a
// new claim: the API has no in-place revert.
func (c *Client) Clone(
	ctx context.Context, snapshotName, cloneName string, source *corev1.PersistentVolumeClaim, labels map[string]string,
) error {
	apiGroup := group

	clone := &corev1.PersistentVolumeClaim{
		Name:      cloneName,
		Namespace: source.Namespace,
		Labels:    labels,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      source.Spec.AccessModes,
			StorageClassName: source.Spec.StorageClassName,
			Resources:        source.Spec.Resources,
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     snapshotName,
			},
		},
	}

	// A bound claim's capacity can exceed what it asked for, and a restore is
	// refused below the snapshot's size, so the larger of the two is requested.
	if capacity, ok := source.Status.Capacity[corev1.ResourceStorage]; ok {
		requested := clone.Spec.Resources.Requests[corev1.ResourceStorage]
		if capacity.Cmp(requested) > 0 {
			clone.Spec.Resources.Requests[corev1.ResourceStorage] = capacity
		}
	}

	_, err := c.kube.CoreV1().PersistentVolumeClaims(source.Namespace).Create(ctx, clone, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create claim %s/%s from snapshot %s: %w",
			source.Namespace, cloneName, snapshotName, err)
	}

	return nil
}

// DeleteSnapshot removes the VolumeSnapshot. What happens to the storage-side
// snapshot follows the VolumeSnapshotClass's deletion policy. A snapshot that
// is already gone is not an error, since a cleanup may have raced this.
func (c *Client) DeleteSnapshot(ctx context.Context, namespace, snapshotName string) error {
	err := c.dyn.Resource(snapshotGVR).Namespace(namespace).Delete(ctx, snapshotName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete VolumeSnapshot %s/%s: %w", namespace, snapshotName, err)
	}

	return nil
}

// DeleteClaim removes the cloned claim, tolerating one that is already gone.
func (c *Client) DeleteClaim(ctx context.Context, namespace, claimName string) error {
	err := c.kube.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, claimName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete claim %s/%s: %w", namespace, claimName, err)
	}

	return nil
}

// DeleteLabeled removes every VolumeSnapshot and claim in the namespace that
// carries the label selector, which is how cleanup finds what a run left
// behind. The claims go first, since a clone holds its snapshot in use.
func (c *Client) DeleteLabeled(ctx context.Context, namespace, labelSelector string) ([]string, error) {
	var removed []string

	claims, err := c.kube.CoreV1().PersistentVolumeClaims(namespace).List(ctx,
		metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("failed to list claims in %s: %w", namespace, err)
	}

	for i := range claims.Items {
		if err := c.DeleteClaim(ctx, namespace, claims.Items[i].Name); err != nil {
			return removed, err
		}

		removed = append(removed, "claim "+claims.Items[i].Name)
	}

	snapshots, err := c.dyn.Resource(snapshotGVR).Namespace(namespace).List(ctx,
		metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		// A cluster without the CRDs has nothing of this kind to remove.
		if apierrors.IsNotFound(err) {
			return removed, nil
		}

		return removed, fmt.Errorf("failed to list snapshots in %s: %w", namespace, err)
	}

	for i := range snapshots.Items {
		if err := c.DeleteSnapshot(ctx, namespace, snapshots.Items[i].GetName()); err != nil {
			return removed, err
		}

		removed = append(removed, "snapshot "+snapshots.Items[i].GetName())
	}

	return removed, nil
}

func (c *Client) boundContentName(
	ctx context.Context, namespace, snapshotName string, lastError *string,
) (string, error) {
	snapshot, err := c.dyn.Resource(snapshotGVR).Namespace(namespace).Get(ctx, snapshotName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get VolumeSnapshot %s/%s: %w", namespace, snapshotName, err)
	}

	noteError(snapshot, lastError)

	contentName, _, _ := unstructured.NestedString(snapshot.Object, "status", "boundVolumeSnapshotContentName")

	return contentName, nil
}

// poll runs condition every pollInterval until it returns true, errors, or the
// timeout passes. The timeout error names what was being waited for, since the
// bare one names neither the wait nor its budget.
func (c *Client) poll(
	ctx context.Context, timeout time.Duration, condition wait.ConditionWithContextFunc, what string,
	lastError *string,
) error {
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, condition)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			if *lastError != "" {
				return fmt.Errorf("timed out after %s waiting for %s; the last error the controller reported: %s",
					timeout, what, *lastError)
			}

			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}

		return err
	}

	return nil
}

// apiInstalled reports whether the cluster serves the snapshot API at all.
//
// This is what separates "that class does not exist" from "this cluster has no
// snapshot API", which the API server answers identically with a 404.
func (c *Client) apiInstalled() bool {
	groups, err := c.kube.Discovery().ServerGroups()
	if err != nil {
		// Undecidable, so assume it is there and let the caller's own error
		// stand rather than claiming something about the cluster.
		return true
	}

	for _, g := range groups.Groups {
		if g.Name != group {
			continue
		}

		for _, version := range g.Versions {
			if version.Version == version1 {
				return true
			}
		}
	}

	return false
}

func toAny(labels map[string]string) map[string]any {
	result := make(map[string]any, len(labels))
	for key, value := range labels {
		result[key] = value
	}

	return result
}
