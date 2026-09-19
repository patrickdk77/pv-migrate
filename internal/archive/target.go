package archive

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Target is a parsed --archive-file value: where the archive lives, and what
// format it is in.
type Target struct {
	// Claim is the PersistentVolumeClaim holding the archive. It is empty when
	// the value named no claim.
	Claim string
	// Bucket is the S3 bucket holding the archive, for the s3:// form. The
	// archive is then streamed to and from the object and never lands on a
	// disk in the pod.
	Bucket string
	// Path is the archive's path: inside the claim, the object key inside the
	// bucket, or a path inside the job pod when neither is set.
	Path string
	// Compression is derived from the path's extension.
	Compression string
}

// InCluster reports whether the archive lives on a claim, which is what decides
// whether the job mounts a second volume.
func (t Target) InCluster() bool {
	return t.Claim != ""
}

// InBucket reports whether the archive is an S3 object, which is what decides
// whether tar is piped through rclone instead of writing a file.
func (t Target) InBucket() bool {
	return t.Bucket != ""
}

// s3Scheme is the prefix of the bucket form, which is read before the claim
// split so "s3" is never taken for a claim name.
const s3Scheme = "s3://"

// suffixes maps an archive's extension to its compression, longest first so
// ".tar.gz" is not read as ".tar" with a stray suffix.
var suffixes = []struct {
	ext         string
	compression string
}{
	{".tar.zst", CompressionZstd},
	{".tzst", CompressionZstd},
	{".tar.gz", CompressionGzip},
	{".tgz", CompressionGzip},
	{".tar", CompressionNone},
}

// Extensions returns the accepted archive file extensions, longest first, for
// help text and error messages.
func Extensions() []string {
	names := make([]string, 0, len(suffixes))
	for _, candidate := range suffixes {
		names = append(names, candidate.ext)
	}

	return names
}

// ParseTarget reads an --archive-file value.
//
// The value is "s3://<bucket>/<key>", "<claim>:<path>" or a bare path. A claim name cannot contain a
// slash or a colon, so text before the first colon is a claim only when it
// holds no slash; anything else is a path, which keeps a Windows-style or
// otherwise colon-bearing path from being read as a claim.
//
// Timestamp tokens in the value are expanded against now before anything else,
// so the caller can report the name it resolved to and the cluster only ever
// sees a literal path.
func ParseTarget(value string, now time.Time) (Target, error) {
	if value == "" {
		return Target{}, errors.New("--archive-file must not be empty")
	}

	expanded, err := expandTime(value, now)
	if err != nil {
		return Target{}, err
	}

	target, err := splitLocation(expanded)
	if err != nil {
		return Target{}, err
	}

	if target.Path == "" {
		return Target{}, errors.New("--archive-file names no path")
	}

	if target.Compression, err = compressionFromPath(target.Path); err != nil {
		return Target{}, err
	}

	return target, nil
}

// compressionFromPath reads the compression off the archive's extension, so the
// file says what it is and a restore does not have to be told again.
func compressionFromPath(path string) (string, error) {
	for _, candidate := range suffixes {
		if strings.HasSuffix(path, candidate.ext) {
			return candidate.compression, nil
		}
	}

	return "", fmt.Errorf("--archive-file %q must end in one of %s",
		path, strings.Join(Extensions(), ", "))
}

// timeTokens maps the strftime-style tokens to Go reference layouts.
var timeTokens = map[byte]string{
	'Y': "2006",
	'm': "01",
	'd': "02",
	'H': "15",
	'M': "04",
	'S': "05",
}

// expandTime replaces the timestamp tokens in value against now. A percent sign
// is doubled to mean itself, and an unknown token is an error rather than being
// passed through, so a typo names a wrong file loudly instead of quietly.
func expandTime(value string, now time.Time) (string, error) {
	var builder strings.Builder

	for index := 0; index < len(value); index++ {
		if value[index] != '%' {
			builder.WriteByte(value[index])

			continue
		}

		if index+1 >= len(value) {
			return "", errors.New("--archive-file ends in a lone %, which names no timestamp token")
		}

		index++

		if value[index] == '%' {
			builder.WriteByte('%')

			continue
		}

		layout, ok := timeTokens[value[index]]
		if !ok {
			return "", fmt.Errorf("--archive-file has unknown timestamp token %%%c, known ones are %s",
				value[index], knownTokens())
		}

		builder.WriteString(now.Format(layout))
	}

	return builder.String(), nil
}

// knownTokens lists the accepted tokens for an error message, in a fixed order
// so the message does not change between runs.
func knownTokens() string {
	ordered := []byte{'Y', 'm', 'd', 'H', 'M', 'S'}

	names := make([]string, 0, len(ordered))
	for _, token := range ordered {
		names = append(names, "%"+string(token))
	}

	return strings.Join(names, ", ")
}

// splitLocation reads where the archive lives off the front of the value: an
// s3:// bucket, a claim before a slash-free colon, or nothing for a bare path.
func splitLocation(value string) (Target, error) {
	if rest, isBucket := strings.CutPrefix(value, s3Scheme); isBucket {
		bucket, key, found := strings.Cut(rest, "/")
		if !found || bucket == "" || key == "" {
			return Target{}, errors.New(
				"--archive-file with s3:// needs a bucket and an object path, as in s3://bucket/path/file.tar.zst")
		}

		return Target{Bucket: bucket, Path: key}, nil
	}

	if claim, path, found := strings.Cut(value, ":"); found && !strings.Contains(claim, "/") {
		if claim == "" {
			return Target{}, errors.New("--archive-file names an empty claim before the colon")
		}

		return Target{Claim: claim, Path: path}, nil
	}

	return Target{Path: value}, nil
}
