package bucketstorage

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/utkuozdemir/pv-migrate/internal/archive"
	"github.com/utkuozdemir/pv-migrate/internal/pvc"
	"github.com/utkuozdemir/pv-migrate/internal/rclone"
)

func refTime() time.Time {
	return time.Date(2026, 9, 14, 3, 7, 9, 0, time.UTC)
}

// pinned returns a claim whose volume may only be used on the given nodes,
// which is what a node-local or zone-scoped volume reports regardless of
// whether anything has it mounted.
func pinned(name string, nodes ...string) *pvc.Info {
	return &pvc.Info{
		Claim:        &corev1.PersistentVolumeClaim{Namespace: "ns", Name: name},
		AllowedNodes: nodes,
		PinnedNodes:  nodes,
	}
}

func info(name, node string, modes ...corev1.PersistentVolumeAccessMode) *pvc.Info {
	result := &pvc.Info{
		Claim:       &corev1.PersistentVolumeClaim{Namespace: "ns", Name: name},
		MountedNode: node,
	}

	for _, mode := range modes {
		switch mode {
		case corev1.ReadWriteOnce, corev1.ReadWriteOncePod:
			result.SupportsRWO = true
		case corev1.ReadOnlyMany:
			result.SupportsROX = true
		case corev1.ReadWriteMany:
			result.SupportsRWX = true
		}
	}

	if node != "" {
		result.AffinityHelmValues = map[string]any{"node": node}

		if result.SupportsRWO && !result.SupportsRWX && !result.SupportsROX {
			result.PinnedNodes = []string{node}
		}
	}

	return result
}

func TestIsArchive(t *testing.T) {
	t.Parallel()

	assert.True(t, isArchive(&Request{ArchiveFile: "nfs:/b/a.tar.zst"}))
	assert.True(t, isArchive(&Request{ArchiveFile: "/tmp/a.tar"}))
	assert.False(t, isArchive(&Request{}))
	assert.False(t, isArchive(&Request{Bucket: "b"}))
}

func TestParseArchiveTarget_OK(t *testing.T) {
	t.Parallel()

	got, err := parseArchiveTarget(&Request{ArchiveFile: "nfs:/backups/db-%Y%m%d.tar.zst"}, refTime())
	require.NoError(t, err)
	assert.Equal(t, "nfs", got.Claim)
	assert.Equal(t, "/backups/db-20260914.tar.zst", got.Path)
	assert.Equal(t, archive.CompressionZstd, got.Compression)
}

func TestParseArchiveTarget_LevelOnCompressedBackupIsAccepted(t *testing.T) {
	t.Parallel()

	got, err := parseArchiveTarget(&Request{
		ArchiveFile: "nfs:/a.tar.zst", CompressionLevel: 19, Direction: rclone.DirectionBackup,
	}, refTime())
	require.NoError(t, err)
	assert.Equal(t, archive.CompressionZstd, got.Compression)
}

func TestParseArchiveTarget_Errors(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		req     Request
		wantErr string
	}{
		"backend also set": {
			req:     Request{ArchiveFile: "nfs:/a.tar", Backend: "s3"},
			wantErr: "--backend cannot be combined with --archive-file",
		},
		"bucket also set": {
			req:     Request{ArchiveFile: "nfs:/a.tar", Bucket: "b"},
			wantErr: "--bucket cannot be combined with --archive-file",
		},
		"rclone config also set": {
			req:     Request{ArchiveFile: "nfs:/a.tar", RcloneConfigFile: "c.conf"},
			wantErr: "--rclone-config cannot be combined with --archive-file",
		},
		"remote also set": {
			req:     Request{ArchiveFile: "nfs:/a.tar", Remote: "r:b"},
			wantErr: "--remote cannot be combined with --archive-file",
		},
		"prefix also set": {
			req:     Request{ArchiveFile: "nfs:/a.tar", Prefix: "p"},
			wantErr: "--prefix cannot be combined with --archive-file",
		},
		"name also set": {
			req:     Request{ArchiveFile: "nfs:/a.tar", Name: "db"},
			wantErr: "--name cannot be combined with --archive-file",
		},
		"archive claim is the source claim": {
			req:     Request{ArchiveFile: "db-data:/a.tar", PVCName: "db-data"},
			wantErr: "names the claim being backed up",
		},
		"level on an uncompressed extension": {
			req:     Request{ArchiveFile: "nfs:/a.tar", CompressionLevel: 3},
			wantErr: "does not apply to \"/a.tar\"",
		},
		"level on a restore": {
			req:     Request{ArchiveFile: "nfs:/a.tar.zst", CompressionLevel: 3, Direction: rclone.DirectionRestore},
			wantErr: "does not apply to a restore",
		},
		"bad extension": {
			req:     Request{ArchiveFile: "nfs:/a.zip"},
			wantErr: "must end in one of",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := parseArchiveTarget(&tc.req, refTime())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestArchiveFilePath(t *testing.T) {
	t.Parallel()

	t.Run("claim path is mount-relative", func(t *testing.T) {
		t.Parallel()

		got, err := archiveFilePath(archive.Target{Claim: "nfs", Path: "/backups/a.tar.zst"})
		require.NoError(t, err)
		assert.Equal(t, "/archive/backups/a.tar.zst", got)
	})

	t.Run("a leading slash is optional", func(t *testing.T) {
		t.Parallel()

		got, err := archiveFilePath(archive.Target{Claim: "nfs", Path: "backups/a.tar"})
		require.NoError(t, err)
		assert.Equal(t, "/archive/backups/a.tar", got)
	})

	t.Run("a bare path is left alone", func(t *testing.T) {
		t.Parallel()

		got, err := archiveFilePath(archive.Target{Path: "/home/me/a.tar"})
		require.NoError(t, err)
		assert.Equal(t, "/home/me/a.tar", got)
	})

	for _, tc := range []struct{ name, path string }{
		{"escapes upward", "/../a.tar"},
		{"escapes deeper", "/backups/../../a.tar"},
		{"names the claim root itself", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := archiveFilePath(archive.Target{Claim: "nfs", Path: tc.path})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must name a file inside the claim")
			assert.Empty(t, got)
		})
	}
}

func TestBuildArchiveCmd_Directions(t *testing.T) {
	t.Parallel()

	target := archive.Target{Claim: "nfs", Path: "/db.tar.zst", Compression: archive.CompressionZstd}

	backup, err := buildArchiveCmd(&Request{Direction: rclone.DirectionBackup}, target, "/data")
	require.NoError(t, err)
	assert.Contains(t, backup, "tar -c ")
	assert.Contains(t, backup, "-f '/archive/db.tar.zst' -C '/data' .")

	restore, err := buildArchiveCmd(&Request{Direction: rclone.DirectionRestore}, target, "/data")
	require.NoError(t, err)
	assert.Contains(t, restore, "tar -x ")
	assert.NotContains(t, restore, "-I ")
}

func TestSidecarPath(t *testing.T) {
	t.Parallel()

	for archivePath, want := range map[string]string{
		"/archive/db.tar.zst": "/archive/db.meta.yaml",
		"/archive/db.tar.gz":  "/archive/db.meta.yaml",
		"/archive/db.tgz":     "/archive/db.meta.yaml",
		"/archive/db.tar":     "/archive/db.meta.yaml",
	} {
		t.Run(archivePath, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, sidecarPath(archivePath))
		})
	}
}

func TestBuildMetadata_RestoreWritesNone(t *testing.T) {
	t.Parallel()

	meta, err := buildMetadata(&Request{
		Direction: rclone.DirectionRestore, ArchiveFile: "nfs:/db.tar.zst",
	}, archive.Target{Claim: "nfs", Path: "/db.tar.zst"}, "ns")
	require.NoError(t, err)
	assert.Empty(t, meta.base64)
	assert.Empty(t, meta.localPath)
}

func TestBuildMetadata_ArchiveBackupWritesLocally(t *testing.T) {
	t.Parallel()

	meta, err := buildMetadata(&Request{
		Direction: rclone.DirectionBackup, ArchiveFile: "nfs:/db.tar.zst", PVCName: "src",
	}, archive.Target{Claim: "nfs", Path: "/db.tar.zst", Compression: archive.CompressionZstd}, "ns")
	require.NoError(t, err)
	assert.NotEmpty(t, meta.base64)
	assert.Equal(t, "/archive/db.meta.yaml", meta.localPath)
	assert.Empty(t, meta.remotePath)
}

func TestCheckSchedulable(t *testing.T) {
	t.Parallel()

	t.Run("neither volume is pinned", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, checkSchedulable(pinned("src"), pinned("dst")))
	})

	t.Run("only one volume is pinned", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, checkSchedulable(pinned("src", "node-a"), pinned("dst")))
		require.NoError(t, checkSchedulable(pinned("src"), pinned("dst", "node-b")))
	})

	t.Run("pinned to the same node", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, checkSchedulable(pinned("src", "node-a"), pinned("dst", "node-a")))
	})

	// A zone-scoped volume resolves to every node in its zone, so two of them
	// overlap wherever the zones do.
	t.Run("overlapping zones", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, checkSchedulable(
			pinned("src", "node-a", "node-b"), pinned("dst", "node-b", "node-c")))
	})

	t.Run("pinned to different nodes is refused", func(t *testing.T) {
		t.Parallel()

		err := checkSchedulable(pinned("src", "node-a"), pinned("dst", "node-b"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "node-a")
		assert.Contains(t, err.Error(), "node-b")
		assert.Contains(t, err.Error(), "one pod cannot mount both")
	})

	t.Run("disjoint zones are refused", func(t *testing.T) {
		t.Parallel()

		err := checkSchedulable(
			pinned("src", "node-a", "node-b"), pinned("dst", "node-c", "node-d"))
		require.Error(t, err)
	})

	// The case a mounted-pod check cannot see. A claim cloned from a snapshot
	// has never been mounted, so nothing reports a node for it, yet its volume
	// is pinned to wherever the snapshot lives.
	t.Run("an unmounted clone is still pinned", func(t *testing.T) {
		t.Parallel()

		clone := pinned("backup-clone", "node-a")
		require.Empty(t, clone.MountedNode, "a fresh clone is mounted nowhere")

		err := checkSchedulable(clone, pinned("archive", "node-b"))
		require.Error(t, err, "its volume's topology still rules out the other claim's node")
	})
}

func TestArchiveAffinity(t *testing.T) {
	t.Parallel()

	t.Run("requires the intersection when pinned", func(t *testing.T) {
		t.Parallel()

		got := archiveAffinity(pinned("src", "node-a", "node-b"), pinned("dst", "node-b"))
		assert.Equal(t, pvc.RequireNodes([]string{"node-b"}), got)
	})

	t.Run("requires the one pinned side when the other is free", func(t *testing.T) {
		t.Parallel()

		got := archiveAffinity(pinned("src"), pinned("dst", "node-c"))
		assert.Equal(t, pvc.RequireNodes([]string{"node-c"}), got)
	})

	t.Run("falls back to a preference when nothing is pinned", func(t *testing.T) {
		t.Parallel()

		data := pinned("src")
		data.AffinityHelmValues = map[string]any{"node": "node-a"}

		assert.Equal(t, data.AffinityHelmValues, archiveAffinity(data, pinned("dst")))
	})

	t.Run("nothing at all", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, archiveAffinity(pinned("src"), pinned("dst")))
	})
}

func TestBuildHelmValues_Archive(t *testing.T) {
	t.Parallel()

	req := &Request{ArchiveFile: "nfs:/db.tar.zst", Direction: rclone.DirectionBackup}
	vals := buildHelmValues("ns", req, info("src", "node-a"), info("nfs", ""),
		"", "tar -c ...", true, metadataValues{base64: "meta", localPath: "/archive/db.meta.yaml"})

	rcloneVals, ok := vals["rclone"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, false, rcloneVals["configMount"],
		"an archive job runs tar and needs no rclone config")
	assert.Equal(t, "/archive/db.meta.yaml", rcloneVals["metadataLocalPath"])
	assert.Empty(t, rcloneVals["metadataRemotePath"])

	mounts, ok := rcloneVals["pvcMounts"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, mounts, 2)

	assert.Equal(t, "src", mounts[0]["name"])
	assert.Equal(t, dataMountPath, mounts[0]["mountPath"])
	assert.Equal(t, true, mounts[0]["readOnly"], "the volume is only read on backup")

	assert.Equal(t, "nfs", mounts[1]["name"])
	assert.Equal(t, archiveMountPath, mounts[1]["mountPath"])
	assert.Equal(t, false, mounts[1]["readOnly"], "the archive is written on backup")
}

func TestBuildHelmValues_ArchiveRestoreInvertsMounts(t *testing.T) {
	t.Parallel()

	req := &Request{ArchiveFile: "nfs:/db.tar.zst", Direction: rclone.DirectionRestore}
	vals := buildHelmValues("ns", req, info("src", ""), info("nfs", ""),
		"", "tar -x ...", false, metadataValues{})

	rcloneVals, ok := vals["rclone"].(map[string]any)
	require.True(t, ok)

	mounts, ok := rcloneVals["pvcMounts"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, mounts, 2)

	assert.Equal(t, false, mounts[0]["readOnly"], "the volume is written on restore")
	assert.Equal(t, true, mounts[1]["readOnly"], "the archive is only read on restore")
}

func TestBuildHelmValues_BucketKeepsOneMount(t *testing.T) {
	t.Parallel()

	vals := buildHelmValues("ns", &Request{Bucket: "b"}, info("src", ""), nil,
		"[remote]\n", "rclone sync ...", true, metadataValues{})

	rcloneVals, ok := vals["rclone"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, true, rcloneVals["configMount"])

	mounts, ok := rcloneVals["pvcMounts"].([]map[string]any)
	require.True(t, ok)
	assert.Len(t, mounts, 1)
}

func s3Request() Request {
	return Request{
		Direction:   rclone.DirectionBackup,
		PVCName:     "mysql-data",
		ArchiveFile: "s3://backups/mysql/db.tar.zst",
		Endpoint:    "https://s3.example.com",
		Region:      "us-east-1",
		AccessKey:   "AKIA",
		SecretKey:   "secret",
	}
}

func TestParseArchiveTarget_S3AllowsTheS3Flags(t *testing.T) {
	t.Parallel()

	req := s3Request()
	req.Backend = rclone.BackendS3
	req.S3Provider = "Minio"

	got, err := parseArchiveTarget(&req, refTime())
	require.NoError(t, err)
	assert.Equal(t, "backups", got.Bucket)
	assert.Equal(t, "mysql/db.tar.zst", got.Path)
}

func TestParseArchiveTarget_S3Errors(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(r *Request){
		"other backend": func(r *Request) { r.Backend = "gcs" },
		"bucket flag":   func(r *Request) { r.Bucket = "b" },
		"prefix flag":   func(r *Request) { r.Prefix = "p" },
		"name flag":     func(r *Request) { r.Name = "n" },
		"rclone config": func(r *Request) { r.RcloneConfigFile = "c.conf" },
		"remote flag":   func(r *Request) { r.Remote = "r:b" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := s3Request()
			mutate(&req)

			_, err := parseArchiveTarget(&req, refTime())
			require.Error(t, err)
		})
	}
}

func TestResolveTarget_S3BuildsConfigAndObjectPath(t *testing.T) {
	t.Parallel()

	req := s3Request()
	target, err := parseArchiveTarget(&req, refTime())
	require.NoError(t, err)

	conf, remotePath, err := resolveTarget(&req, target)
	require.NoError(t, err)
	assert.Contains(t, conf, "type = s3")
	assert.Contains(t, conf, "https://s3.example.com")
	assert.Equal(t, "remote:backups/mysql/db.tar.zst", remotePath)
}

func TestResolveTarget_S3RejectsBadBucket(t *testing.T) {
	t.Parallel()

	req := s3Request()
	req.ArchiveFile = "s3://bad bucket/db.tar.zst"
	target, err := parseArchiveTarget(&req, refTime())
	require.NoError(t, err)

	_, _, err = resolveTarget(&req, target)
	require.Error(t, err)
}

func TestBuildMoverCmd_S3Streams(t *testing.T) {
	t.Parallel()

	req := s3Request()
	target := archive.Target{Bucket: "backups", Path: "mysql/db.tar.zst", Compression: archive.CompressionZstd}

	backup, err := buildMoverCmd(&req, target, "/data", "remote:backups/mysql/db.tar.zst")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(backup, "set -o pipefail; used=$(df -B1 '/data'"), backup)
	assert.Contains(t, backup, "; tar -c ")
	assert.Contains(t, backup, "| rclone --config '/etc/rclone/rclone.conf'")
	assert.Contains(t, backup, "rcat 'remote:backups/mysql/db.tar.zst'")

	req.Direction = rclone.DirectionRestore
	restore, err := buildMoverCmd(&req, target, "/data", "remote:backups/mysql/db.tar.zst")
	require.NoError(t, err)
	assert.Contains(t, restore, "cat 'remote:backups/mysql/db.tar.zst' | tar -x ")
	assert.Contains(t, restore, "-I 'zstd'")
}

func TestBuildMetadata_S3UploadsNextToTheObject(t *testing.T) {
	t.Parallel()

	req := s3Request()
	target := archive.Target{Bucket: "backups", Path: "mysql/db.tar.zst", Compression: archive.CompressionZstd}

	meta, err := buildMetadata(&req, target, "ns")
	require.NoError(t, err)
	assert.NotEmpty(t, meta.base64)
	assert.Equal(t, "remote:backups/mysql/db.meta.yaml", meta.remotePath)
	assert.Empty(t, meta.localPath, "an S3 archive has no volume to write a sidecar to")
}

// The config is mounted when there is one to mount, which an S3 archive has
// and a tar to a volume does not.
func TestBuildHelmValues_ConfigMountFollowsTheConfig(t *testing.T) {
	t.Parallel()

	withConf := buildHelmValues(
		"ns",
		&Request{},
		info("src", ""),
		nil,
		"[remote]\ntype = s3\n",
		"cmd",
		true,
		metadataValues{},
	)
	withoutConf := buildHelmValues("ns", &Request{}, info("src", ""), nil, "", "cmd", true, metadataValues{})

	assert.Equal(t, true, withConf["rclone"].(map[string]any)["configMount"])     //nolint:forcetypeassert
	assert.Equal(t, false, withoutConf["rclone"].(map[string]any)["configMount"]) //nolint:forcetypeassert
}

// The public API and the CLI fill --prefix with a default before the request
// is built, so the default must not read as a chosen prefix or every
// --archive-file invocation is refused.
func TestParseArchiveTarget_DefaultedPrefixIsNotAConflict(t *testing.T) {
	t.Parallel()

	_, err := parseArchiveTarget(&Request{ArchiveFile: "nfs:/a.tar", Prefix: DefaultPrefix}, refTime())
	require.NoError(t, err)

	_, err = parseArchiveTarget(&Request{ArchiveFile: "nfs:/a.tar", Prefix: "chosen"}, refTime())
	require.Error(t, err)
}

func TestParseArchiveTarget_MoreConflicts(t *testing.T) {
	t.Parallel()

	t.Run("delete-extraneous-files on an archive restore", func(t *testing.T) {
		t.Parallel()

		_, err := parseArchiveTarget(&Request{
			ArchiveFile: "nfs:/a.tar", Direction: rclone.DirectionRestore, DeleteExtraneousFiles: true,
		}, refTime())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--delete-extraneous-files")
	})

	t.Run("delete-extraneous-files on an archive backup is ignored", func(t *testing.T) {
		t.Parallel()

		_, err := parseArchiveTarget(&Request{
			ArchiveFile: "nfs:/a.tar", Direction: rclone.DirectionBackup, DeleteExtraneousFiles: true,
		}, refTime())
		require.NoError(t, err)
	})

	t.Run("rclone extra args on a file form", func(t *testing.T) {
		t.Parallel()

		_, err := parseArchiveTarget(&Request{ArchiveFile: "nfs:/a.tar", RcloneExtraArgs: "--bwlimit 1M"}, refTime())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--rclone-extra-args")
	})

	t.Run("rclone extra args on the s3 form are allowed", func(t *testing.T) {
		t.Parallel()

		req := s3Request()
		req.RcloneExtraArgs = "--s3-chunk-size 64M"
		_, err := parseArchiveTarget(&req, refTime())
		require.NoError(t, err)
	})

	t.Run("out of range level is refused up front", func(t *testing.T) {
		t.Parallel()

		_, err := parseArchiveTarget(&Request{ArchiveFile: "nfs:/a.tar.zst", CompressionLevel: 25}, refTime())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of range")
	})
}

func TestBuildMoverCmd_S3ForwardsExtraArgs(t *testing.T) {
	t.Parallel()

	req := s3Request()
	req.RcloneExtraArgs = "--s3-chunk-size 64M"
	target := archive.Target{Bucket: "backups", Path: "mysql/db.tar.zst", Compression: archive.CompressionZstd}

	backup, err := buildMoverCmd(&req, target, "/data", "remote:backups/mysql/db.tar.zst")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(backup, `--s3-chunk-size "${chunk}M" --s3-chunk-size 64M`), backup)
}

func TestBuildMetadata_S3DryRunWritesNothing(t *testing.T) {
	t.Parallel()

	req := s3Request()
	req.RcloneExtraArgs = "--dry-run"
	target := archive.Target{Bucket: "backups", Path: "mysql/db.tar.zst", Compression: archive.CompressionZstd}

	meta, err := buildMetadata(&req, target, "ns")
	require.NoError(t, err)
	assert.Empty(t, meta.base64)
	assert.Empty(t, meta.remotePath)
}

// A ReadWriteMany claim mounted somewhere is only a preference, since its
// volume can attach elsewhere too. A ReadWriteOnce one is pinned to where it
// is attached, and that hard limit has to win.
func TestArchiveAffinity_AttachedReadWriteOnceOutranksAPreference(t *testing.T) {
	t.Parallel()

	data := info("src", "node-a", corev1.ReadWriteMany)
	require.Nil(t, data.PinnedNodes, "a ReadWriteMany volume is not pinned by being mounted")

	archiveClaim := info("dst", "node-b", corev1.ReadWriteOnce)
	require.Equal(t, []string{"node-b"}, archiveClaim.PinnedNodes)

	assert.Equal(t, pvc.RequireNodes([]string{"node-b"}), archiveAffinity(data, archiveClaim))
}

// The job's name ends in its mover, which is how the exit-code table and the
// progress parser are picked for it once it has run.
func TestJobSuffix(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "tar", jobSuffix(&Request{ArchiveFile: "nfs:/a.tar"}))
	assert.Equal(t, "tar", jobSuffix(&Request{ArchiveFile: "s3://b/a.tar.zst"}))
	assert.Equal(t, "rclone", jobSuffix(&Request{Bucket: "b"}))

	vals := buildHelmValues("ns", &Request{ArchiveFile: "nfs:/a.tar"}, info("src", ""), info("nfs", ""),
		"", "tar ...", true, metadataValues{})
	assert.Equal(t, "tar", vals["rclone"].(map[string]any)["jobSuffix"]) //nolint:forcetypeassert
}
