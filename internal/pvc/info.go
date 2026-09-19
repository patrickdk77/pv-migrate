package pvc

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"

	"github.com/utkuozdemir/pv-migrate/internal/k8s"
)

type Info struct {
	ClusterClient *k8s.ClusterClient
	Claim         *corev1.PersistentVolumeClaim
	MountedNode   string
	// AllowedNodes are the nodes the claim's bound volume may be used on,
	// resolved from that volume's node affinity. It is nil when the volume
	// places no constraint, or when the claim is not bound yet.
	//
	// This, not MountedNode, is the real limit. A volume carries its affinity
	// whether or not anything has it mounted, and what it pins to need not be
	// a node: a cloud disk pins to its availability zone, a node-local volume
	// to one node. Resolving the selector against the node list expresses
	// either as the set of nodes that can use it.
	AllowedNodes []string
	// PinnedNodes is AllowedNodes narrowed by where the volume is already
	// attached, which is the set a pod using this claim must land in. Nil
	// means nothing constrains it.
	PinnedNodes []string
	// MountedPod is the pod that has the claim mounted, empty when none does.
	// For a database volume it is the database itself, which is what a
	// quiesce has to talk to.
	MountedPod         string
	AffinityHelmValues map[string]any
	SupportsRWO        bool
	SupportsROX        bool
	SupportsRWX        bool
}

func New(
	ctx context.Context,
	client *k8s.ClusterClient,
	ns, name string,
) (*Info, error) {
	return newInfo(ctx, client, ns, name, true)
}

// NewSource resolves a claim that will be snapshotted rather than mounted. A
// mounted ReadWriteOncePod claim is refused by New because a second pod could
// never mount it, but a snapshot reads it through a clone and never mounts it,
// so the refusal does not apply.
func NewSource(
	ctx context.Context,
	client *k8s.ClusterClient,
	ns, name string,
) (*Info, error) {
	return newInfo(ctx, client, ns, name, false)
}

func newInfo(
	ctx context.Context,
	client *k8s.ClusterClient,
	ns, name string,
	rejectMountedRWOP bool,
) (*Info, error) {
	kubeClient := client.KubeClient

	claim, err := kubeClient.CoreV1().PersistentVolumeClaims(ns).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pvc %s/%s: %w", ns, name, err)
	}

	supportsRWO, supportsROX, supportsRWX, readWriteOncePod := readAccessModes(claim)

	mountedPod, mountedNode, err := findMountedPod(ctx, kubeClient, claim)
	if err != nil {
		return nil, err
	}

	if rejectMountedRWOP && readWriteOncePod && mountedNode != "" {
		return nil, fmt.Errorf("pvc %s/%s is mounted to a pod and has ReadWriteOncePod "+
			"access mode, it cannot be mounted to the migration pod", ns, name)
	}

	attachedToOneNode := !supportsRWX && !supportsROX

	allowedNodes, pinned, err := resolveTopology(ctx, kubeClient, claim, mountedNode, attachedToOneNode)
	if err != nil {
		return nil, err
	}

	affinityHelmValues := buildAffinityHelmValues(mountedNode, pinned)

	return &Info{
		ClusterClient:      client,
		Claim:              claim,
		MountedNode:        mountedNode,
		MountedPod:         mountedPod,
		AllowedNodes:       allowedNodes,
		PinnedNodes:        pinned,
		AffinityHelmValues: affinityHelmValues,
		SupportsRWO:        supportsRWO,
		SupportsROX:        supportsROX,
		SupportsRWX:        supportsRWX,
	}, nil
}

// Describe says what the claim is in one line: where it is, how big, which
// access modes, and whether a pod has it mounted.
func (i *Info) Describe() string {
	claim := i.Claim
	line := claim.Namespace + "/" + claim.Name

	if size := i.Size(); !size.IsZero() {
		line += ", " + size.String()
	}

	modes := make([]string, 0, len(claim.Spec.AccessModes))
	for _, mode := range claim.Spec.AccessModes {
		modes = append(modes, string(mode))
	}

	if len(modes) > 0 {
		line += ", " + strings.Join(modes, "+")
	}

	if i.MountedNode != "" {
		return line + ", mounted on node " + i.MountedNode
	}

	return line + ", not mounted"
}

// Size returns the storage capacity of the PVC. It prefers the actual capacity
// of the bound PersistentVolume (Status.Capacity) and falls back to the
// requested capacity (Spec.Resources.Requests) when the PVC is not yet bound.
// It returns a zero quantity when neither is set.
func (i *Info) Size() resource.Quantity {
	if i == nil || i.Claim == nil {
		return resource.Quantity{}
	}

	if capacity, ok := i.Claim.Status.Capacity[corev1.ResourceStorage]; ok && !capacity.IsZero() {
		return capacity
	}

	return i.Claim.Spec.Resources.Requests[corev1.ResourceStorage]
}

const (
	// storageProvisionerAnnotation is set on a PVC by the controller to record
	// which provisioner handles its dynamic provisioning.
	storageProvisionerAnnotation = "volume.kubernetes.io/storage-provisioner"
	// betaStorageProvisionerAnnotation is the pre-1.23 form of the above; some
	// clusters still carry it.
	betaStorageProvisionerAnnotation = "volume.beta.kubernetes.io/storage-provisioner"
)

// Provisioner resolves the name of the storage provisioner backing the PVC on a
// best-effort basis. It first reads the provisioner annotation set on the claim,
// which is present once a dynamically provisioned PVC has been picked up by the
// controller, then falls back to the provisioner of the claim's StorageClass.
// The fallback is needed for PVCs that are not yet bound (e.g. those using the
// WaitForFirstConsumer volume binding mode), whose annotation is not set yet.
// It returns an empty name when the provisioner cannot be determined; the
// returned error is non-nil only when a StorageClass lookup was attempted and
// failed, so callers may treat it as best-effort.
func (i *Info) Provisioner(ctx context.Context) (string, error) {
	if i == nil || i.Claim == nil {
		return "", nil
	}

	if provisioner := i.Claim.Annotations[storageProvisionerAnnotation]; provisioner != "" {
		return provisioner, nil
	}

	if provisioner := i.Claim.Annotations[betaStorageProvisionerAnnotation]; provisioner != "" {
		return provisioner, nil
	}

	storageClassName := i.Claim.Spec.StorageClassName
	if storageClassName == nil || *storageClassName == "" {
		return "", nil
	}

	if i.ClusterClient == nil || i.ClusterClient.KubeClient == nil {
		return "", nil
	}

	storageClass, err := i.ClusterClient.KubeClient.StorageV1().
		StorageClasses().Get(ctx, *storageClassName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get storage class %s: %w", *storageClassName, err)
	}

	return storageClass.Provisioner, nil
}

// findMountedPod returns the name and node of the pod that has the claim
// mounted, or empty strings when no pod does.
func findMountedPod(ctx context.Context, kubeClient kubernetes.Interface,
	pvc *corev1.PersistentVolumeClaim,
) (string, string, error) {
	podList, err := kubeClient.CoreV1().Pods(pvc.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range podList.Items {
		// A finished pod still lists its volumes but holds nothing mounted,
		// and would pin the job to a node the claim has left, or hand a flush
		// a database that is not running.
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		for _, volume := range pod.Spec.Volumes {
			persistentVolumeClaim := volume.PersistentVolumeClaim
			if persistentVolumeClaim != nil && persistentVolumeClaim.ClaimName == pvc.Name {
				return pod.Name, pod.Spec.NodeName, nil
			}
		}
	}

	return "", "", nil
}

// buildAffinityHelmValues turns what a claim permits into affinity values.
//
// A volume that names the nodes it can be used on is a hard requirement: the
// scheduler will refuse anything else anyway, and saying so up front is what
// lets a conflict be reported instead of leaving a pod Pending. A claim that
// is merely mounted somewhere, while its volume could be attached elsewhere,
// is only a preference.
func buildAffinityHelmValues(mountedNode string, pinned []string) map[string]any {
	if pinned != nil {
		return RequireNodes(pinned)
	}

	if mountedNode == "" {
		return nil
	}

	return map[string]any{
		"nodeAffinity": map[string]any{
			"preferredDuringSchedulingIgnoredDuringExecution": []map[string]any{
				{
					"weight": 100, //nolint:mnd
					"preference": map[string]any{
						"matchFields": []map[string]any{
							{
								"key":      "metadata.name",
								"operator": "In",
								"values":   []string{mountedNode},
							},
						},
					},
				},
			},
		},
	}
}

// readAccessModes reports which access modes a claim asks for, and whether one
// of them is the single-pod variant.
//
//nolint:nonamedreturns // four booleans read better named than positional
func readAccessModes(claim *corev1.PersistentVolumeClaim) (rwo, rox, rwx, rwop bool) {
	for _, accessMode := range claim.Spec.AccessModes {
		switch accessMode {
		case corev1.ReadWriteOncePod:
			rwo = true
			rwop = true
		case corev1.ReadWriteOnce:
			rwo = true
		case corev1.ReadOnlyMany:
			rox = true
		case corev1.ReadWriteMany:
			rwx = true
		}
	}

	return rwo, rox, rwx, rwop
}

// resolveTopology returns what the claim's volume permits, and that narrowed
// by where the volume is already attached when only one node may attach it.
func resolveTopology(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	claim *corev1.PersistentVolumeClaim,
	mountedNode string,
	attachedToOneNode bool,
) (allowed, pinned []string, err error) {
	allowed, err = AllowedNodesFor(ctx, kubeClient, claim)
	if err != nil {
		return nil, nil, err
	}

	if allowed != nil && len(allowed) == 0 {
		return nil, nil, fmt.Errorf(
			"the volume of claim %s/%s is bound to nodes that no longer exist in this cluster, "+
				"so nothing can mount it", claim.Namespace, claim.Name)
	}

	pinned = allowed
	if mountedNode != "" && attachedToOneNode {
		pinned = IntersectNodes(pinned, []string{mountedNode})
	}

	return allowed, pinned, nil
}

// AllowedNodesFor resolves the nodes a claim's volume may be used on, or nil
// when it is unconstrained or not bound yet.
//
// The answer comes from the bound PersistentVolume's node affinity, which is
// what the CSI driver published as the volume's real topology. It is a node
// selector rather than a node name, so a zone-scoped volume resolves to every
// node in its zone and a node-local one to a single node, without this code
// needing to know which kind it is looking at.
func AllowedNodesFor(
	ctx context.Context, kubeClient kubernetes.Interface, claim *corev1.PersistentVolumeClaim,
) ([]string, error) {
	if claim.Spec.VolumeName == "" {
		return nil, nil
	}

	selector, constrained, err := volumeNodeSelector(ctx, kubeClient, claim)
	if err != nil || !constrained {
		return nil, err
	}

	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Empty rather than nil when nothing matches: the volume is constrained
	// and no node satisfies it, which is a different answer from having no
	// constraint at all, and callers act on that difference.
	allowed := []string{}

	for i := range nodes.Items {
		if selector.Match(&nodes.Items[i]) {
			allowed = append(allowed, nodes.Items[i].Name)
		}
	}

	return allowed, nil
}

// volumeNodeSelector returns the node selector a claim's bound volume carries,
// and whether there was one to read at all.
//
// Volumes are cluster-scoped and a namespace-scoped account is not required to
// read them. Losing the topology costs the early conflict check, not
// correctness, since the scheduler still enforces a volume's affinity, so a
// refused read is treated as "unknown" rather than failing the operation.
func volumeNodeSelector(
	ctx context.Context, kubeClient kubernetes.Interface, claim *corev1.PersistentVolumeClaim,
) (*nodeaffinity.NodeSelector, bool, error) {
	volume, err := kubeClient.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("failed to get volume %s of claim %s/%s: %w",
			claim.Spec.VolumeName, claim.Namespace, claim.Name, err)
	}

	if volume.Spec.NodeAffinity == nil || volume.Spec.NodeAffinity.Required == nil {
		return nil, false, nil
	}

	selector, err := nodeaffinity.NewNodeSelector(volume.Spec.NodeAffinity.Required)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read the node affinity of volume %s: %w", volume.Name, err)
	}

	return selector, true, nil
}

// IntersectNodes returns the nodes both constraints allow. A nil constraint
// means unconstrained, so it yields the other one.
func IntersectNodes(first, second []string) []string {
	if first == nil {
		return second
	}

	if second == nil {
		return first
	}

	inFirst := make(map[string]struct{}, len(first))
	for _, name := range first {
		inFirst[name] = struct{}{}
	}

	// Non-nil and empty is a real answer: nothing satisfies both.
	both := []string{}

	for _, name := range second {
		if _, ok := inFirst[name]; ok {
			both = append(both, name)
		}
	}

	return both
}

// RequireNodes builds Helm affinity values pinning a pod to the given nodes.
func RequireNodes(nodes []string) map[string]any {
	// Nil is unconstrained. An empty set is not: it means nothing satisfies
	// the constraint, which callers refuse before reaching this.
	if nodes == nil {
		return nil
	}

	return map[string]any{
		"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []map[string]any{{
					"matchFields": []map[string]any{{
						"key":      "metadata.name",
						"operator": "In",
						"values":   nodes,
					}},
				}},
			},
		},
	}
}
