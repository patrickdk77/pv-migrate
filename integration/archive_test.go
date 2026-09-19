//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neilotoole/slogt/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/utkuozdemir/pv-migrate/internal/k8s"
	"github.com/utkuozdemir/pv-migrate/pvmigrate"
)

const (
	// archiveMountPath is where the claim holding the archive is mounted in
	// the inspection pod, which is the same path provisionPod uses.
	archiveInspectPath = "/volume"
	archiveDataContent = "ARCHIVE_DATA"
)

// archiveSeedCmd seeds a volume with the things a tar preserves and an rclone
// file sync does not: a non-default mode, a non-root owner, a hard link and a
// sparse file. A round trip that keeps all four is the reason the archive
// workflow exists.
func archiveSeedCmd() string {
	return strings.Join([]string{
		"mkdir -p /volume/sub",
		fmt.Sprintf("echo -n %s > /volume/archive_test.txt", archiveDataContent),
		"chmod 640 /volume/archive_test.txt",
		"chown 999:999 /volume/archive_test.txt",
		"ln /volume/archive_test.txt /volume/hardlink.txt",
		"echo -n DEEP > /volume/sub/deep.txt",
		"chmod 600 /volume/sub/deep.txt",
		"chown 999:999 /volume/sub",
		"dd if=/dev/zero of=/volume/sparse.img bs=1 count=0 seek=1M",
	}, " && ")
}

// fingerprint returns one line per entry of a mounted volume, carrying the
// facts a file sync loses. Two volumes with the same fingerprint hold the same
// tree with the same ownership and modes.
func fingerprint(t *testing.T, infra *backupTestInfra, ns, pod string) string {
	t.Helper()

	out, err := execInPod(t.Context(), infra.cli, ns, pod,
		"cd "+archiveInspectPath+` && find . -mindepth 1 -exec stat -c '%a %u %g %n' {} \; | sort`)
	require.NoError(t, err)

	return strings.TrimSpace(out)
}

// hardlinkPreserved reports whether the two seeded names still share an inode,
// which tar keeps and a file-by-file copy does not.
func hardlinkPreserved(t *testing.T, infra *backupTestInfra, ns, pod string) bool {
	t.Helper()

	out, err := execInPod(t.Context(), infra.cli, ns, pod,
		"cd "+archiveInspectPath+
			` && [ "$(stat -c %i archive_test.txt)" = "$(stat -c %i hardlink.txt)" ] && echo yes || echo no`)
	require.NoError(t, err)

	return strings.TrimSpace(out) == "yes"
}

// waitForFirstConsumerClass creates a storage class that provisions a volume
// on the node its first consumer is scheduled to, and returns its name.
//
// This suite pins pods to chosen nodes, and only this binding mode makes the
// volume follow. With the immediate binding the default classes use, the
// provisioner picks the node itself and the pinned pod then fails its
// NodeAffinity check, which is a property of the cluster rather than anything
// this project does.
func waitForFirstConsumerClass(t *testing.T, cli *k8s.ClusterClient) string {
	t.Helper()

	source := os.Getenv("PVMIG_TEST_STORAGE_CLASS")
	if source == "" {
		skipOrFail(t, "PVMIG_TEST_STORAGE_CLASS is not set, so there is no provisioner to copy",
			envMultiNodeMustRun)
	}

	existing, err := cli.KubeClient.StorageV1().StorageClasses().Get(t.Context(), source, metav1.GetOptions{})
	if err != nil {
		skipOrFail(t, "cannot read storage class "+source+": "+err.Error(), envMultiNodeMustRun)
	}

	binding := storagev1.VolumeBindingWaitForFirstConsumer
	name := "pvmig-wffc-" + randomSuffix()

	class := &storagev1.StorageClass{
		Name: name, Labels: resourceLabels,
		Provisioner:       existing.Provisioner,
		Parameters:        existing.Parameters,
		ReclaimPolicy:     existing.ReclaimPolicy,
		VolumeBindingMode: &binding,
	}

	createCtx, createCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer createCancel()

	if _, err = cli.KubeClient.StorageV1().
		StorageClasses().
		Create(createCtx, class, metav1.CreateOptions{}); err != nil {
		skipOrFail(t, "cannot create a WaitForFirstConsumer storage class: "+err.Error(), envMultiNodeMustRun)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := cli.KubeClient.StorageV1().StorageClasses().Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			t.Logf("failed to delete storage class %q: %v", name, err)
		}
	})

	return name
}

// provisionPinnedPod is provisionPod with the pod placed on a named node, so a
// test can decide which node a ReadWriteOnce claim ends up bound to.
func provisionPinnedPod(
	ctx context.Context, cli *k8s.ClusterClient, ns, pvcName, podName, node, class, seedCmd string,
) error {
	return provisionPinnedPodWithMode(ctx, cli, ns, pvcName, podName, node, class, seedCmd,
		corev1.ReadWriteOnce)
}

// provisionPinnedPodWithMode is provisionPinnedPod with the claim's access
// mode chosen by the caller.
func provisionPinnedPodWithMode(
	ctx context.Context, cli *k8s.ClusterClient, ns, pvcName, podName, node, class, seedCmd string,
	mode corev1.PersistentVolumeAccessMode,
) error {
	storageClassRef := &class

	claim := corev1.PersistentVolumeClaim{
		Name:      pvcName,
		Namespace: ns,
		Labels:    resourceLabels,
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: storageClassRef,
			AccessModes:      []corev1.PersistentVolumeAccessMode{mode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{"storage": resource.MustParse("64Mi")},
			},
		},
	}

	if _, err := cli.KubeClient.CoreV1().PersistentVolumeClaims(ns).
		Create(ctx, &claim, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create PVC %s/%s: %w", ns, pvcName, err)
	}

	grace := int64(0)
	mounts := []corev1.VolumeMount{{Name: "volume", MountPath: "/volume"}}

	pod := corev1.Pod{
		Name:      podName,
		Namespace: ns,
		Labels:    resourceLabels,
		Spec: corev1.PodSpec{
			// A node selector rather than spec.nodeName: setting the node
			// directly bypasses the scheduler, and WaitForFirstConsumer
			// binding is triggered by the scheduler, so the volume would
			// never be provisioned and the pod would wait on it forever.
			NodeSelector:                  map[string]string{corev1.LabelHostname: node},
			TerminationGracePeriodSeconds: &grace,
			Volumes: []corev1.Volume{{
				Name:                  "volume",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
			}},
			Containers: []corev1.Container{{
				Name:         "main",
				Image:        "docker.io/busybox:stable",
				Command:      []string{"tail", "-f", "/dev/null"},
				VolumeMounts: mounts,
			}},
		},
	}

	if seedCmd != "" {
		pod.Spec.InitContainers = []corev1.Container{{
			Name:         "seed",
			Image:        "docker.io/busybox:stable",
			Command:      []string{"sh", "-c", seedCmd},
			VolumeMounts: mounts,
		}}
	}

	if _, err := cli.KubeClient.CoreV1().Pods(ns).Create(ctx, &pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create pod %s/%s on node %s: %w", ns, podName, node, err)
	}

	return waitPodRunning(ctx, cli, ns, podName)
}

// placementNode returns the node a test should put all of its claims on.
//
// These suites are not testing scheduling; they need their claims mountable by
// one pod. On a cluster with node-local storage and more than one node, claims
// left to the provisioner land on different nodes and nothing can mount them
// together, so everything a test creates goes on one node deliberately.
func placementNode(t *testing.T, cli *k8s.ClusterClient) string {
	t.Helper()

	nodes := schedulableNodes(t, cli)
	require.NotEmpty(t, nodes, "a cluster needs at least one schedulable node")

	return nodes[0]
}

// placement carries the node and storage class a test provisions everything
// with, so its claims can all be mounted by one pod.
type placement struct {
	node  string
	class string
}

// newPlacement picks the node and makes the class. The class binds on first
// consumer, which is what makes the node choice stick.
func newPlacement(t *testing.T, cli *k8s.ClusterClient) placement {
	t.Helper()

	return placement{node: placementNode(t, cli), class: waitForFirstConsumerClass(t, cli)}
}

// seedPlacedPVC is seedAndCreatePVC with the claim placed, returning the
// namespace.
func seedPlacedPVC(t *testing.T, infra *backupTestInfra, p placement, seedCmd string) string {
	t.Helper()

	ns := newTestNS(t, infra.cli, "pvmig-bak")
	require.NoError(t, provisionPinnedPod(t.Context(), infra.cli, ns,
		"test-pvc", "test-pod", p.node, p.class, seedCmd))

	return ns
}

// placePod provisions a further claim and pod alongside, on the same node.
func placePod(t *testing.T, infra *backupTestInfra, p placement, ns, pvcName, podName string) {
	t.Helper()

	require.NoError(t, provisionPinnedPod(t.Context(), infra.cli, ns, pvcName, podName, p.node, p.class, ""))
}

// archiveBackup builds a backup that writes one tar file, with the image
// override the suite was given.
func archiveBackup(t *testing.T, infra *backupTestInfra, ns, archiveFile string) pvmigrate.Backup {
	t.Helper()

	return pvmigrate.Backup{
		PVC:                pvmigrate.PVC{Namespace: ns, Name: "test-pvc"},
		ArchiveFile:        archiveFile,
		IgnoreMounted:      true,
		NoCleanupOnFailure: true,
		HelmValues:         infra.rcloneHelmVals,
		Logger:             slogt.New(t),
		Writer:             os.Stderr,
	}
}

func archiveRestore(t *testing.T, infra *backupTestInfra, ns, pvcName, archiveFile string) pvmigrate.Restore {
	t.Helper()

	return pvmigrate.Restore{
		PVC:                pvmigrate.PVC{Namespace: ns, Name: pvcName},
		ArchiveFile:        archiveFile,
		IgnoreMounted:      true,
		NoCleanupOnFailure: true,
		HelmValues:         infra.rcloneHelmVals,
		Logger:             slogt.New(t),
		Writer:             os.Stderr,
	}
}

func TestArchive(t *testing.T) {
	t.Parallel()

	infra := setupBackupInfra(t)

	cases := map[string]func(*testing.T, *backupTestInfra){
		"ClaimRoundTripPreservesMetadata": testArchiveClaimRoundTrip,
		"CompressionVariants":             testArchiveCompressionVariants,
		"TimestampTokensExpand":           testArchiveTimestampTokens,
		"SidecarWrittenBesideArchive":     testArchiveSidecar,
		"InPodPathWithMountedVolume":      testArchiveInPodPath,
		"S3RoundTrip":                     testArchiveS3RoundTrip,
		"S3MultipartUpload":               testArchiveS3Multipart,
		"MissingArchiveFails":             testArchiveMissingFileFails,
		"NonRootSkipsLostFound":           testArchiveNonRootLostFound,
	}

	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t, infra)
		})
	}
}

// testArchiveClaimRoundTrip is the case the whole workflow exists for: a
// volume written to one tar file on another claim and read back with its
// ownership, modes, hard links and sparseness intact.
//
//nolint:thelper // subtest implementation, not a helper
func testArchiveClaimRoundTrip(t *testing.T, infra *backupTestInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra, place, archiveSeedCmd())

	placePod(t, infra, place, ns, "archive-pvc", "archive-pod")
	placePod(t, infra, place, ns, "dest-pvc", "dest-pod")

	before := fingerprint(t, infra, ns, "test-pod")
	require.True(t, hardlinkPreserved(t, infra, ns, "test-pod"), "the seed itself should have a hard link")

	archiveFile := "archive-pvc:/backups/db.tar.zst"

	require.NoError(t, pvmigrate.RunBackup(t.Context(), archiveBackup(t, infra, ns, archiveFile)))

	listing, err := execInPod(t.Context(), infra.cli, ns, "archive-pod", "ls "+archiveInspectPath+"/backups")
	require.NoError(t, err)
	assert.Contains(t, listing, "db.tar.zst")

	require.NoError(t, pvmigrate.RunRestore(t.Context(), archiveRestore(t, infra, ns, "dest-pvc", archiveFile)))

	assert.Equal(t, before, fingerprint(t, infra, ns, "dest-pod"),
		"an archive must restore ownership and modes, which a file sync does not")
	assert.True(t, hardlinkPreserved(t, infra, ns, "dest-pod"), "the hard link must survive the round trip")

	blocks, err := execInPod(t.Context(), infra.cli, ns, "dest-pod", "stat -c %b "+archiveInspectPath+"/sparse.img")
	require.NoError(t, err)
	assert.Less(t, atoiOrZero(strings.TrimSpace(blocks)), 64,
		"a sparse file must come back sparse rather than as its full length of zeroes")
}

// testArchiveNonRootLostFound is the case a bind-mounted test volume cannot
// produce on its own.
//
// Every ext4 or xfs volume carries a root-owned lost+found at mode 700 that a
// non-root mover cannot read, and without an exclude tar stops with code 2
// over a directory holding no user data. The CI storage is a directory rather
// than a filesystem and has none, so the seed makes one instead of hoping the
// storage does.
//
// The seed is its own rather than archiveSeedCmd's, which exists to prove
// metadata survives and therefore plants files only uid 999 can read. Those
// defeat a non-root run on their own and would say nothing about lost+found.
//
// It stops at the archive rather than restoring, and reads the members back,
// because that is what this exclude decides. A non-root restore also has to
// set modes on a volume root it does not own, which is a separate limit and
// not this case's subject.
//
//nolint:thelper // subtest implementation, not a helper
func testArchiveNonRootLostFound(t *testing.T, infra *backupTestInfra) {
	place := newPlacement(t, infra.cli)

	seed := strings.Join([]string{
		"mkdir -p /volume/sub/lost+found",
		"echo -n DATA > /volume/data.txt",
		"echo -n KEEP > /volume/sub/lost+found/mine",
		"chmod -R a+rX /volume/data.txt /volume/sub",
		// What mkfs leaves at the root of a real filesystem.
		"mkdir -p /volume/lost+found",
		"echo -n RECOVERED > /volume/lost+found/orphan",
		"chown -R 0:0 /volume/lost+found",
		"chmod 700 /volume/lost+found",
	}, " && ")

	ns := seedPlacedPVC(t, infra, place, seed)
	placePod(t, infra, place, ns, "archive-pvc", "archive-pod")

	// Uncompressed, so the busybox in the inspection pod can list the members.
	backup := archiveBackup(t, infra, ns, "archive-pvc:/db.tar")
	backup.NonRoot = true

	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup),
		"a non-root backup must not fail over the filesystem's own recovery directory")

	listing, err := execInPod(t.Context(), infra.cli, ns, "archive-pod",
		"tar -tf "+archiveInspectPath+"/db.tar")
	require.NoError(t, err)

	members := make(map[string]bool)
	for line := range strings.SplitSeq(listing, "\n") {
		members[strings.TrimSpace(line)] = true
	}

	assert.False(t, members["./lost+found/"] || members["./lost+found/orphan"],
		"the volume's own recovery directory is not user data and must not be archived")
	assert.True(t, members["./data.txt"], "the rest of the volume still has to be archived")
	assert.True(t, members["./sub/lost+found/mine"],
		"the exclude is anchored, so a directory a user calls lost+found further down is still theirs")
}

// testArchiveCompressionVariants pins that the extension alone selects the
// compression, in both directions, for every accepted suffix.
//
//nolint:thelper // subtest implementation, not a helper
func testArchiveCompressionVariants(t *testing.T, infra *backupTestInfra) {
	for _, extension := range []string{".tar.zst", ".tar.gz", ".tar"} {
		t.Run(extension, func(t *testing.T) {
			t.Parallel()

			place := newPlacement(t, infra.cli)
			ns := seedPlacedPVC(t, infra, place, archiveSeedCmd())
			placePod(t, infra, place, ns, "archive-pvc", "archive-pod")
			placePod(t, infra, place, ns, "dest-pvc", "dest-pod")

			before := fingerprint(t, infra, ns, "test-pod")
			archiveFile := "archive-pvc:/db" + extension

			require.NoError(t, pvmigrate.RunBackup(t.Context(), archiveBackup(t, infra, ns, archiveFile)))
			require.NoError(t,
				pvmigrate.RunRestore(t.Context(), archiveRestore(t, infra, ns, "dest-pvc", archiveFile)))

			assert.Equal(t, before, fingerprint(t, infra, ns, "dest-pod"))
		})
	}
}

// testArchiveTimestampTokens checks that the tokens are expanded by the client
// before anything reaches the cluster, so the file lands under a resolved name
// rather than a literal one.
//
//nolint:thelper // subtest implementation, not a helper
func testArchiveTimestampTokens(t *testing.T, infra *backupTestInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra, place, archiveSeedCmd())
	placePod(t, infra, place, ns, "archive-pvc", "archive-pod")

	require.NoError(t, pvmigrate.RunBackup(t.Context(),
		archiveBackup(t, infra, ns, "archive-pvc:/db-%Y-%m-%d.tar.zst")))

	listing, err := execInPod(t.Context(), infra.cli, ns, "archive-pod", "ls "+archiveInspectPath)
	require.NoError(t, err)

	assert.NotContains(t, listing, "%Y", "the token must be expanded, not written literally")
	assert.Regexp(t, `db-\d{4}-\d{2}-\d{2}\.tar\.zst`, listing)
}

// testArchiveSidecar checks the metadata lands beside the archive, named after
// it, so the pair is obvious in a listing.
//
//nolint:thelper // subtest implementation, not a helper
func testArchiveSidecar(t *testing.T, infra *backupTestInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra, place, archiveSeedCmd())
	placePod(t, infra, place, ns, "archive-pvc", "archive-pod")

	require.NoError(t, pvmigrate.RunBackup(t.Context(),
		archiveBackup(t, infra, ns, "archive-pvc:/db.tar.zst")))

	sidecar, err := execInPod(t.Context(), infra.cli, ns, "archive-pod",
		"cat "+archiveInspectPath+"/db.meta.yaml")
	require.NoError(t, err)

	assert.Contains(t, sidecar, "sourcePvc: test-pvc")
	assert.Contains(t, sidecar, "format: tar")
	assert.Contains(t, sidecar, "compression: zstd")
}

// testArchiveInPodPath covers the bare-path form: a path with no claim in
// front of it is a path inside the job's own container, which only works when
// the caller mounts a volume there. The Helm values list both mounts, because
// a user-supplied list replaces the computed one rather than adding to it.
//
//nolint:thelper // subtest implementation, not a helper
func testArchiveInPodPath(t *testing.T, infra *backupTestInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra, place, archiveSeedCmd())
	placePod(t, infra, place, ns, "archive-pvc", "archive-pod")

	backup := archiveBackup(t, infra, ns, "/mnt/out/db.tar.zst")
	backup.HelmValues = append(backup.HelmValues,
		"rclone.pvcMounts[0].name=test-pvc",
		"rclone.pvcMounts[0].mountPath=/data",
		"rclone.pvcMounts[0].readOnly=true",
		"rclone.pvcMounts[1].name=archive-pvc",
		"rclone.pvcMounts[1].mountPath=/mnt/out",
	)

	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup))

	listing, err := execInPod(t.Context(), infra.cli, ns, "archive-pod", "ls "+archiveInspectPath)
	require.NoError(t, err)
	assert.Contains(t, listing, "db.tar.zst", "the archive should land on the volume mounted at that path")
}

// testArchiveS3RoundTrip streams the archive into one object and reads it back
// through the same pipe, which is the path that never lands the file on a disk
// in the pod.
//
//nolint:thelper // subtest implementation, not a helper
func testArchiveS3RoundTrip(t *testing.T, infra *backupTestInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra, place, archiveSeedCmd())
	placePod(t, infra, place, ns, "dest-pvc", "dest-pod")

	before := fingerprint(t, infra, ns, "test-pod")

	key := "archive/" + ns + "/db.tar.zst"
	archiveFile := "s3://" + minioBucket + "/" + key

	backup := archiveBackup(t, infra, ns, archiveFile)
	backup.Endpoint = minioEndpoint
	backup.AccessKey = minioAccessKey
	backup.SecretKey = minioSecretKey

	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup))

	assert.Contains(t, listBucketPath(t, infra, ns, "archive/"+ns+"/"), "db.tar.zst")
	assert.Contains(t, readBucketFile(t, infra, ns, "archive/"+ns+"/db.meta.yaml"), "format: tar",
		"the sidecar is uploaded beside the object")

	restore := archiveRestore(t, infra, ns, "dest-pvc", archiveFile)
	restore.Endpoint = minioEndpoint
	restore.AccessKey = minioAccessKey
	restore.SecretKey = minioSecretKey

	require.NoError(t, pvmigrate.RunRestore(t.Context(), restore))

	assert.Equal(t, before, fingerprint(t, infra, ns, "dest-pod"),
		"a streamed archive must preserve what a file sync loses, the same as one on a claim")
}

// testArchiveMissingFileFails pins that a restore of an archive that is not
// there fails, rather than reporting success over an empty volume.
//
//nolint:thelper // subtest implementation, not a helper
func testArchiveMissingFileFails(t *testing.T, infra *backupTestInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra, place, archiveSeedCmd())
	placePod(t, infra, place, ns, "archive-pvc", "archive-pod")
	placePod(t, infra, place, ns, "dest-pvc", "dest-pod")

	restore := archiveRestore(t, infra, ns, "dest-pvc", "archive-pvc:/never-written.tar.zst")
	restore.NoCleanupOnFailure = false

	require.Error(t, pvmigrate.RunRestore(t.Context(), restore),
		"restoring an archive that does not exist must fail")
}

// multipartSeedBytes is comfortably past rclone's 100 KiB
// streaming-upload-cutoff, so an upload of unknown size leaves the single-PUT
// path and is sent as chunks. The data is incompressible on purpose: a
// compressible payload would shrink below the cutoff and test nothing.
const multipartSeedBytes = 20 * 1024 * 1024

// testArchiveS3Multipart pushes an object big enough that rclone has to chunk
// it, which is the path the computed --s3-chunk-size exists for, and checks
// the bytes survive it. A unit test can assert the flag is on the command; only
// this can show the upload works when it is used.
//
//nolint:thelper // subtest implementation, not a helper
func testArchiveS3Multipart(t *testing.T, infra *backupTestInfra) {
	seed := fmt.Sprintf(
		"dd if=/dev/urandom of=/volume/blob bs=1M count=%d 2>/dev/null && md5sum /volume/blob > /volume/blob.md5",
		multipartSeedBytes/(1024*1024))

	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra, place, seed)
	placePod(t, infra, place, ns, "dest-pvc", "dest-pod")

	before, err := execInPod(t.Context(), infra.cli, ns, "test-pod", "cat /volume/blob.md5")
	require.NoError(t, err)

	// An uncompressed archive keeps the stream incompressible all the way to
	// rclone, so the object really is large.
	key := "multipart/" + ns + "/blob.tar"
	archiveFile := "s3://" + minioBucket + "/" + key

	backup := archiveBackup(t, infra, ns, archiveFile)
	backup.Endpoint = minioEndpoint
	backup.AccessKey = minioAccessKey
	backup.SecretKey = minioSecretKey

	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup))

	listing := listBucketPath(t, infra, ns, "multipart/"+ns+"/")
	assert.Contains(t, listing, "blob.tar")
	assert.Contains(t, listing, "MiB", "the object should be large enough to have been chunked: %s", listing)

	restore := archiveRestore(t, infra, ns, "dest-pvc", archiveFile)
	restore.Endpoint = minioEndpoint
	restore.AccessKey = minioAccessKey
	restore.SecretKey = minioSecretKey

	require.NoError(t, pvmigrate.RunRestore(t.Context(), restore))

	after, err := execInPod(t.Context(), infra.cli, ns, "dest-pod",
		"cd /volume && md5sum -c blob.md5 && cat blob.md5")
	require.NoError(t, err, "the restored blob must match the checksum taken before the upload")
	assert.Contains(t, after, strings.Fields(before)[0])
}

func atoiOrZero(s string) int {
	value := 0

	for _, char := range s {
		if char < '0' || char > '9' {
			return value
		}

		value = value*10 + int(char-'0')
	}

	return value
}
