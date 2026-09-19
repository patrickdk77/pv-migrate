package archive_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/utkuozdemir/pv-migrate/internal/archive"
)

func TestBuild_BackupZstd(t *testing.T) {
	t.Parallel()

	cmd := archive.Cmd{
		Direction:   archive.DirectionBackup,
		ArchivePath: "/dest/my-pvc.tar.zst",
		DataPath:    "/data",
		Compression: archive.CompressionZstd,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.Equal(
		t,
		"mkdir -p '/dest' && tar -c --numeric-owner --xattrs --sparse -I 'zstd -T0 -3' "+
			"-f '/dest/my-pvc.tar.zst' -C '/data' .",
		result,
	)
}

func TestBuild_BackupGzipWithLevel(t *testing.T) {
	t.Parallel()

	cmd := archive.Cmd{
		Direction:   archive.DirectionBackup,
		ArchivePath: "/dest/my-pvc.tar.gz",
		DataPath:    "/data",
		Compression: archive.CompressionGzip,
		Level:       9,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.Contains(t, result, "-I 'gzip -9'")
}

func TestBuild_BackupUncompressed(t *testing.T) {
	t.Parallel()

	cmd := archive.Cmd{
		Direction:   archive.DirectionBackup,
		ArchivePath: "/dest/my-pvc.tar",
		DataPath:    "/data",
		Compression: archive.CompressionNone,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.NotContains(t, result, "-I ")
	assert.Equal(
		t,
		"mkdir -p '/dest' && tar -c --numeric-owner --xattrs --sparse -f '/dest/my-pvc.tar' -C '/data' .",
		result,
	)
}

func TestBuild_Restore(t *testing.T) {
	t.Parallel()

	cmd := archive.Cmd{
		Direction:   archive.DirectionRestore,
		ArchivePath: "/dest/my-pvc.tar.zst",
		DataPath:    "/data",
		Compression: archive.CompressionZstd,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.Equal(
		t,
		"mkdir -p '/data' && tar -x --numeric-owner --xattrs -f '/dest/my-pvc.tar.zst' -C '/data'",
		result,
	)
	// tar detects the compression from the archive itself, so naming one here
	// could only contradict the file.
	assert.NotContains(t, result, "-I ")
	// The data directory is created, since --path can name one that does not
	// exist yet; the archive's own directory is not, as it is being read.
	assert.True(t, strings.HasPrefix(result, "mkdir -p '/data' && "), result)
	// A restore must not append the member selector, which on extract would
	// limit the restore to that one path instead of the whole archive.
	assert.False(t, strings.HasSuffix(result, " ."))
}

// The archive's directory is created so a first backup into a fresh
// subdirectory does not fail on a path that does not exist yet.
func TestBuild_BackupCreatesTheArchiveDirectory(t *testing.T) {
	t.Parallel()

	cmd := archive.Cmd{
		Direction:   archive.DirectionBackup,
		ArchivePath: "/archive/backups/2026/db.tar.zst",
		DataPath:    "/data",
		Compression: archive.CompressionZstd,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result, "mkdir -p '/archive/backups/2026' && tar -c "), result)
}

func TestBuild_QuotesPathsWithSpaces(t *testing.T) {
	t.Parallel()

	cmd := archive.Cmd{
		Direction:   archive.DirectionBackup,
		ArchivePath: "/dest/my backup.tar.zst",
		DataPath:    "/data/some dir",
		Compression: archive.CompressionZstd,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.Contains(t, result, "-f '/dest/my backup.tar.zst'")
	assert.Contains(t, result, "-C '/data/some dir'")
}

func TestBuild_QuotesPathWithSingleQuote(t *testing.T) {
	t.Parallel()

	cmd := archive.Cmd{
		Direction:   archive.DirectionBackup,
		ArchivePath: "/dest/o'brien.tar",
		DataPath:    "/data",
		Compression: archive.CompressionNone,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.Contains(t, result, `'/dest/o'"'"'brien.tar'`)
}

type errCase struct {
	mutate  func(c *archive.Cmd)
	wantErr string
}

// validCmd is the command every error case starts from, so each case states
// only the one thing it makes wrong.
func validCmd() archive.Cmd {
	return archive.Cmd{
		Direction:   archive.DirectionBackup,
		ArchivePath: "/dest/a.tar",
		DataPath:    "/data",
		Compression: archive.CompressionNone,
	}
}

func buildErrorCases() map[string]errCase {
	return map[string]errCase{
		"empty direction": {
			func(c *archive.Cmd) { c.Direction = "" }, "invalid direction",
		},
		"unknown direction": {
			func(c *archive.Cmd) { c.Direction = "sideways" }, "invalid direction",
		},
		"empty archive path": {
			func(c *archive.Cmd) { c.ArchivePath = "" }, "--name must not be empty",
		},
		"empty data path": {
			func(c *archive.Cmd) { c.DataPath = "" }, "--path must not be empty",
		},
		"newline in archive path": {
			func(c *archive.Cmd) { c.ArchivePath = "/dest/a\nb.tar" }, "--name must not contain",
		},
		"newline in data path": {
			func(c *archive.Cmd) { c.DataPath = "/data\nx" }, "--path must not contain",
		},
		"invalid utf-8 in archive path": {
			func(c *archive.Cmd) { c.ArchivePath = "/dest/\xff.tar" }, "--name must be valid UTF-8",
		},
		"unsupported compression": {
			func(c *archive.Cmd) { c.Compression = "bzip2" }, "unsupported compression",
		},
		"zstd level too high": {
			func(c *archive.Cmd) { c.Compression = archive.CompressionZstd; c.Level = 20 }, "out of range",
		},
		"zstd level negative": {
			func(c *archive.Cmd) { c.Compression = archive.CompressionZstd; c.Level = -1 }, "out of range",
		},
		"gzip level too high": {
			func(c *archive.Cmd) { c.Compression = archive.CompressionGzip; c.Level = 10 }, "out of range",
		},
		"level given for uncompressed": {
			func(c *archive.Cmd) { c.Level = 3 }, "does not apply",
		},
	}
}

// TestValidCmdIsValid keeps the error table honest. Every case there mutates
// validCmd and asserts a failure, so a validCmd that failed on its own would
// make all of them pass without testing anything.
func TestValidCmdIsValid(t *testing.T) {
	t.Parallel()

	cmd := validCmd()

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.NotEmpty(t, result)
}

func TestBuild_Errors(t *testing.T) {
	t.Parallel()

	for name, tc := range buildErrorCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := validCmd()
			tc.mutate(&cmd)

			result, err := cmd.Build()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, result)
		})
	}
}

func TestExtension(t *testing.T) {
	t.Parallel()

	for compression, want := range map[string]string{
		archive.CompressionNone: ".tar",
		archive.CompressionZstd: ".tar.zst",
		archive.CompressionGzip: ".tar.gz",
	} {
		t.Run(compression, func(t *testing.T) {
			t.Parallel()

			got, err := archive.Extension(compression)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestExtension_Unsupported(t *testing.T) {
	t.Parallel()

	got, err := archive.Extension("xz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported compression")
	assert.Empty(t, got)
}

func TestDefaultLevel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, archive.DefaultZstdLevel, archive.DefaultLevel(archive.CompressionZstd))
	assert.Equal(t, archive.DefaultGzipLevel, archive.DefaultLevel(archive.CompressionGzip))
	assert.Equal(t, 0, archive.DefaultLevel(archive.CompressionNone))
	assert.Equal(t, 0, archive.DefaultLevel("nonsense"))
}

const streamRclone = "rclone --config '/etc/rclone/rclone.conf' " +
	"--stats 1s --stats-log-level NOTICE --use-json-log --stats-one-line"

func TestStreamBuild_BackupZstd(t *testing.T) {
	t.Parallel()

	cmd := archive.StreamCmd{
		Direction:   archive.DirectionBackup,
		RemotePath:  "remote:backups/db.tar.zst",
		DataPath:    "/data",
		ConfigPath:  "/etc/rclone/rclone.conf",
		Compression: archive.CompressionZstd,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.Equal(t,
		"set -o pipefail; "+
			`used=$(df -B1 '/data' | awk 'NR==2{print $3}'); `+
			`chunk=$(( (used * 5 / 4 / 9000 / 1048576) + 1 )); [ "$chunk" -lt 5 ] && chunk=5; `+
			"tar -c --numeric-owner --xattrs --sparse -I 'zstd -T0 -3' -f - -C '/data' . | "+
			streamRclone+` rcat 'remote:backups/db.tar.zst' --s3-chunk-size "${chunk}M"`,
		result)
}

func TestStreamBuild_BackupUncompressedHasNoCompressor(t *testing.T) {
	t.Parallel()

	cmd := archive.StreamCmd{
		Direction:   archive.DirectionBackup,
		RemotePath:  "remote:backups/db.tar",
		DataPath:    "/data",
		ConfigPath:  "/etc/rclone/rclone.conf",
		Compression: archive.CompressionNone,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.NotContains(t, result, "-I ")
	assert.True(t, strings.HasPrefix(result, "set -o pipefail; used=$(df -B1 '/data'"), result)
	assert.Contains(t, result, "; tar -c ")
}

// The chunk is sized from the volume's used bytes so the stream fits under
// the part limit assuming nothing compresses; a restore uploads nothing and
// sizes nothing.
func TestStreamBuild_ChunkSizingOnlyOnBackup(t *testing.T) {
	t.Parallel()

	backup := validStreamCmd()
	result, err := backup.Build()
	require.NoError(t, err)
	assert.Contains(t, result, "df -B1 '/data'")
	assert.Contains(t, result, `--s3-chunk-size "${chunk}M"`)

	restore := validStreamCmd()
	restore.Direction = archive.DirectionRestore
	result, err = restore.Build()
	require.NoError(t, err)
	assert.NotContains(t, result, "df -B1")
	assert.NotContains(t, result, "s3-chunk-size")
}

// A pipe is not seekable, so a restore has to name the decompressor; tar
// cannot sniff it as it does from a file.
func TestStreamBuild_RestoreNamesTheDecompressor(t *testing.T) {
	t.Parallel()

	for compression, want := range map[string]string{
		archive.CompressionZstd: " -I 'zstd' ",
		archive.CompressionGzip: " -I 'gzip' ",
	} {
		t.Run(compression, func(t *testing.T) {
			t.Parallel()

			cmd := archive.StreamCmd{
				Direction:   archive.DirectionRestore,
				RemotePath:  "remote:backups/db.tar.x",
				DataPath:    "/data",
				ConfigPath:  "/etc/rclone/rclone.conf",
				Compression: compression,
			}

			result, err := cmd.Build()
			require.NoError(t, err)
			assert.Contains(t, result, want)
		})
	}
}

func TestStreamBuild_RestoreZstdExact(t *testing.T) {
	t.Parallel()

	cmd := archive.StreamCmd{
		Direction:   archive.DirectionRestore,
		RemotePath:  "remote:backups/db.tar.zst",
		DataPath:    "/data",
		ConfigPath:  "/etc/rclone/rclone.conf",
		Compression: archive.CompressionZstd,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.Equal(t,
		"set -o pipefail; mkdir -p '/data' && "+streamRclone+" cat 'remote:backups/db.tar.zst' | "+
			"tar -x --numeric-owner --xattrs -I 'zstd' -f - -C '/data'",
		result)
}

func TestStreamBuild_RestoreUncompressedHasNoDecompressor(t *testing.T) {
	t.Parallel()

	cmd := archive.StreamCmd{
		Direction:   archive.DirectionRestore,
		RemotePath:  "remote:backups/db.tar",
		DataPath:    "/data",
		ConfigPath:  "/etc/rclone/rclone.conf",
		Compression: archive.CompressionNone,
	}

	result, err := cmd.Build()
	require.NoError(t, err)
	assert.NotContains(t, result, "-I ")
}

func validStreamCmd() archive.StreamCmd {
	return archive.StreamCmd{
		Direction:   archive.DirectionBackup,
		RemotePath:  "remote:backups/db.tar",
		DataPath:    "/data",
		ConfigPath:  "/etc/rclone/rclone.conf",
		Compression: archive.CompressionNone,
	}
}

func TestValidStreamCmdIsValid(t *testing.T) {
	t.Parallel()

	cmd := validStreamCmd()
	_, err := cmd.Build()
	require.NoError(t, err)
}

func TestStreamBuild_Errors(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mutate  func(c *archive.StreamCmd)
		wantErr string
	}{
		"empty remote":      {func(c *archive.StreamCmd) { c.RemotePath = "" }, "--archive-file must not be empty"},
		"empty data path":   {func(c *archive.StreamCmd) { c.DataPath = "" }, "--path must not be empty"},
		"empty config":      {func(c *archive.StreamCmd) { c.ConfigPath = "" }, "rclone config path must not be empty"},
		"newline in remote": {func(c *archive.StreamCmd) { c.RemotePath = "remote:a\nb" }, "must not contain"},
		"bad direction":     {func(c *archive.StreamCmd) { c.Direction = "up" }, "invalid direction"},
		"bad compression":   {func(c *archive.StreamCmd) { c.Compression = "lz4" }, "unsupported compression"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := validStreamCmd()
			tc.mutate(&cmd)

			result, err := cmd.Build()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, result)
		})
	}
}

// Extra rclone args land on the rclone side of the pipe, in both directions,
// so --s3-chunk-size can lift the 48 GiB cap on a streamed upload.
func TestStreamBuild_ForwardsExtraArgs(t *testing.T) {
	t.Parallel()

	backup := validStreamCmd()
	backup.ExtraArgs = "--s3-chunk-size 64M"

	result, err := backup.Build()
	require.NoError(t, err)
	// The user's flag comes after the computed one, and rclone keeps the last.
	assert.True(
		t,
		strings.HasSuffix(result, `rcat 'remote:backups/db.tar' --s3-chunk-size "${chunk}M" --s3-chunk-size 64M`),
		result,
	)

	restore := validStreamCmd()
	restore.Direction = archive.DirectionRestore
	restore.ExtraArgs = "--bwlimit 1M"

	result, err = restore.Build()
	require.NoError(t, err)
	assert.Contains(t, result, "cat 'remote:backups/db.tar' --bwlimit 1M | tar -x")
}

func TestCheckLevel(t *testing.T) {
	t.Parallel()

	require.NoError(t, archive.CheckLevel(archive.CompressionZstd, 19))
	require.NoError(t, archive.CheckLevel(archive.CompressionZstd, 0))
	require.Error(t, archive.CheckLevel(archive.CompressionZstd, 25))
	require.Error(t, archive.CheckLevel(archive.CompressionGzip, 10))
	require.Error(t, archive.CheckLevel(archive.CompressionNone, 3))
}
