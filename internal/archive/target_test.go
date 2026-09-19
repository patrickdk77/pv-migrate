package archive_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/utkuozdemir/pv-migrate/internal/archive"
)

func refTime() time.Time {
	return time.Date(2026, 9, 14, 3, 7, 9, 0, time.UTC)
}

func TestParseTarget_ClaimAndPath(t *testing.T) {
	t.Parallel()

	got, err := archive.ParseTarget("nfs:/backups/mysql.tar.zst", refTime())
	require.NoError(t, err)
	assert.Equal(t, "nfs", got.Claim)
	assert.Equal(t, "/backups/mysql.tar.zst", got.Path)
	assert.Equal(t, archive.CompressionZstd, got.Compression)
	assert.True(t, got.InCluster())
}

func TestParseTarget_BarePathIsNotInCluster(t *testing.T) {
	t.Parallel()

	got, err := archive.ParseTarget("/home/me/mysql.tar.gz", refTime())
	require.NoError(t, err)
	assert.Empty(t, got.Claim)
	assert.Equal(t, "/home/me/mysql.tar.gz", got.Path)
	assert.Equal(t, archive.CompressionGzip, got.Compression)
	assert.False(t, got.InCluster())
}

// A path carrying a colon after a slash is still a path. Reading it as a claim
// would send the archive to a claim that does not exist.
func TestParseTarget_ColonAfterSlashIsPath(t *testing.T) {
	t.Parallel()

	got, err := archive.ParseTarget("/mnt/odd:name/mysql.tar", refTime())
	require.NoError(t, err)
	assert.Empty(t, got.Claim)
	assert.Equal(t, "/mnt/odd:name/mysql.tar", got.Path)
}

func TestParseTarget_CompressionFromExtension(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]string{
		"/a/b.tar.zst": archive.CompressionZstd,
		"/a/b.tzst":    archive.CompressionZstd,
		"/a/b.tar.gz":  archive.CompressionGzip,
		"/a/b.tgz":     archive.CompressionGzip,
		"/a/b.tar":     archive.CompressionNone,
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			got, err := archive.ParseTarget(path, refTime())
			require.NoError(t, err)
			assert.Equal(t, want, got.Compression)
		})
	}
}

func TestParseTarget_ExpandsTimestamps(t *testing.T) {
	t.Parallel()

	got, err := archive.ParseTarget("nfs:/backups/mysql-%Y-%m-%d_%H%M.tar.zst", refTime())
	require.NoError(t, err)
	assert.Equal(t, "nfs", got.Claim)
	assert.Equal(t, "/backups/mysql-2026-09-14_0307.tar.zst", got.Path)
}

func TestParseTarget_ExpandsSeconds(t *testing.T) {
	t.Parallel()

	got, err := archive.ParseTarget("/b/mysql-%Y%m%d%H%M%S.tar", refTime())
	require.NoError(t, err)
	assert.Equal(t, "/b/mysql-20260914030709.tar", got.Path)
}

func TestParseTarget_DoubledPercentIsLiteral(t *testing.T) {
	t.Parallel()

	got, err := archive.ParseTarget("/b/100%%-mysql.tar", refTime())
	require.NoError(t, err)
	assert.Equal(t, "/b/100%-mysql.tar", got.Path)
}

// Without expansion the value has no timestamp. Nothing is added implicitly.
func TestParseTarget_NoTokensMeansNoTimestamp(t *testing.T) {
	t.Parallel()

	got, err := archive.ParseTarget("nfs:/backups/mysql.tar.zst", refTime())
	require.NoError(t, err)
	assert.Equal(t, "/backups/mysql.tar.zst", got.Path)
}

func TestParseTarget_Errors(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		value   string
		wantErr string
	}{
		"empty": {
			value: "", wantErr: "must not be empty",
		},
		"no path after the claim": {
			value: "nfs:", wantErr: "names no path",
		},
		"empty claim before the colon": {
			value: ":/backups/a.tar", wantErr: "empty claim",
		},
		"unknown extension": {
			value: "nfs:/backups/a.zip", wantErr: "must end in one of",
		},
		"no extension": {
			value: "nfs:/backups/a", wantErr: "must end in one of",
		},
		"tar in the middle is not a suffix": {
			value: "nfs:/backups/a.tar.bak", wantErr: "must end in one of",
		},
		"lone trailing percent": {
			value: "/b/a%", wantErr: "lone %",
		},
		"unknown token": {
			value: "/b/a-%Q.tar", wantErr: "unknown timestamp token %Q",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := archive.ParseTarget(tc.value, refTime())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, got.Path)
		})
	}
}

func TestParseTarget_S3(t *testing.T) {
	t.Parallel()

	got, err := archive.ParseTarget("s3://backups/mysql/db-%Y%m%d.tar.zst", refTime())
	require.NoError(t, err)
	assert.Equal(t, "backups", got.Bucket)
	assert.Equal(t, "mysql/db-20260914.tar.zst", got.Path)
	assert.Equal(t, archive.CompressionZstd, got.Compression)
	assert.Empty(t, got.Claim)
	assert.True(t, got.InBucket())
	assert.False(t, got.InCluster())
}

// A claim really named s3, written with one colon, is still a claim. Only the
// scheme with its double slash means a bucket.
func TestParseTarget_ClaimNamedS3IsNotABucket(t *testing.T) {
	t.Parallel()

	got, err := archive.ParseTarget("s3:/a.tar", refTime())
	require.NoError(t, err)
	assert.Equal(t, "s3", got.Claim)
	assert.Empty(t, got.Bucket)
}

func TestParseTarget_S3Errors(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"scheme alone":       "s3://",
		"bucket without key": "s3://backups",
		"empty bucket":       "s3:///a.tar",
		"empty key":          "s3://backups/",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := archive.ParseTarget(value, refTime())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "needs a bucket and an object path")
		})
	}
}
