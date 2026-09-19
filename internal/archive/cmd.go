// Package archive builds the tar command strings that the archive Job runs
// through `sh -c`.
//
// The compressor runs as tar's own child through --use-compress-program rather
// than on the other end of a pipe. A pipeline would report the exit status of
// its last command, so a tar that died half way through feeding a compressor
// that exited cleanly would be recorded as a successful backup. As a child, a
// failing compressor fails tar itself.
package archive

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/utkuozdemir/pv-migrate/internal/shell"
)

const (
	DirectionBackup  = "backup"
	DirectionRestore = "restore"

	CompressionNone = "none"
	CompressionZstd = "zstd"
	CompressionGzip = "gzip"

	// DefaultZstdLevel is zstd's own default, fast enough to keep a spinning
	// disk saturated.
	DefaultZstdLevel = 3
	// DefaultGzipLevel is gzip's own default.
	DefaultGzipLevel = 6

	minLevel     = 1
	maxZstdLevel = 19
	maxGzipLevel = 9

	// compressThreads is how many threads a compressor is given.
	//
	// Not every core, for two reasons. zstd stops scaling early: measured on
	// 937 MiB at level 3, four threads reach 2.7x and thirty-two reach 3.3x,
	// because at that speed the reader becomes the bottleneck long before the
	// compressor does. And a container sees the node's core count rather than
	// its own CPU limit, so "every core" on a large node means dozens of
	// threads contending for a fraction of one. pigz does keep scaling, but
	// zstd on four threads already beats pigz on thirty-two, so a volume that
	// wants speed should be using zstd rather than more gzip threads.
	compressThreads = 4
)

// Compressions returns the accepted compression names, in the order they are
// offered to the user.
func Compressions() []string {
	return []string{CompressionNone, CompressionZstd, CompressionGzip}
}

// Extension returns the file name suffix for an archive written with the given
// compression.
func Extension(compression string) (string, error) {
	switch compression {
	case CompressionNone:
		return ".tar", nil
	case CompressionZstd:
		return ".tar.zst", nil
	case CompressionGzip:
		return ".tar.gz", nil
	default:
		return "", unsupportedCompression(compression)
	}
}

// DefaultLevel returns the compression level used when none is requested. It is
// zero for CompressionNone, which takes no level.
func DefaultLevel(compression string) int {
	switch compression {
	case CompressionZstd:
		return DefaultZstdLevel
	case CompressionGzip:
		return DefaultGzipLevel
	default:
		return 0
	}
}

// Cmd holds the parameters for building a tar command string.
type Cmd struct {
	Direction   string
	ArchivePath string
	DataPath    string
	Compression string
	Level       int
	// Clean empties the data path before extracting, so that a restore
	// replaces the volume rather than merging into it. Restore only.
	Clean bool
}

// Build produces the full tar command string.
func (c *Cmd) Build() (string, error) {
	if err := c.checkPaths(); err != nil {
		return "", err
	}

	program, err := compressProgram(c.Compression, c.Level)
	if err != nil {
		return "", err
	}

	var builder strings.Builder

	switch c.Direction {
	case DirectionBackup:
		// tar opens the archive path as given and does not create the
		// directory above it, so a first backup into a fresh subdirectory
		// would fail before reading a byte. The directory is made here, on
		// the same line, since the job template carries the command as one.
		fmt.Fprintf(&builder, "mkdir -p %s && ", shell.Quote(path.Dir(c.ArchivePath)))
		// --sparse keeps a preallocated database file from being read and
		// stored as its full length of zeroes.
		builder.WriteString("tar -c --numeric-owner --xattrs --sparse " + lostFoundExclude)
	case DirectionRestore:
		// --path can name a directory that does not exist yet on the volume,
		// and tar extracts into a directory rather than creating it.
		fmt.Fprintf(&builder, "mkdir -p %s && ", shell.Quote(c.DataPath))

		if c.Clean {
			fmt.Fprintf(&builder, "%s && ", cleanCmd(c.DataPath))
		}

		builder.WriteString("tar -x --numeric-owner --xattrs " + lostFoundExclude)
	default:
		return "", fmt.Errorf("invalid direction: %q, must be %q or %q",
			c.Direction, DirectionBackup, DirectionRestore)
	}

	// Only a create names its compressor. tar reads the format back from the
	// archive's own magic bytes, so an extract that named one could disagree
	// with what is in the file; left out, a wrong --compression can only miss
	// the file name and fail cleanly.
	if program != "" && c.Direction == DirectionBackup {
		fmt.Fprintf(&builder, " -I %s", shell.Quote(program))
	}

	fmt.Fprintf(&builder, " -f %s -C %s", shell.Quote(c.ArchivePath), shell.Quote(c.DataPath))

	if c.Direction == DirectionBackup {
		builder.WriteString(" .")
	}

	return builder.String(), nil
}

// lostFoundExclude keeps the filesystem's own recovery directory out of the
// archive, anchored so a directory a user happens to call lost+found further
// down is still carried.
//
// Every ext4 or xfs volume has one at its root, owned by root at mode 700, so
// a non-root mover cannot read it on backup or create it on restore and the
// run fails over a directory that holds no user data. It is excluded whatever
// the mover runs as, because an archive whose contents depend on that could
// not be restored by the other one.
const lostFoundExclude = "--exclude=./lost+found"

// cleanCmd empties dataPath, leaving the filesystem's own lost+found alone so
// that the clean agrees with lostFoundExclude: the archive never carried that
// directory, so removing it would be a loss the restore could not undo, and a
// non-root mover cannot remove it anyway.
//
// It is chained with && by its callers on purpose. find exits non-zero when it
// cannot enter a directory, which a non-root mover hits on one it does not
// own, and by then it has already deleted what it could reach. Stopping there
// reports a failed restore; carrying on would extract over a half-emptied
// volume and call it a success, which is the merge this option exists to
// prevent, only worse.
func cleanCmd(dataPath string) string {
	return fmt.Sprintf("find %s -mindepth 1 -not -path %s -not -path %s -delete",
		shell.Quote(dataPath),
		shell.Quote(path.Join(dataPath, "lost+found")),
		shell.Quote(path.Join(dataPath, "lost+found")+"/*"),
	)
}

// checkPaths rejects a path the built command could not carry. Both paths carry
// their flag names so an error points at what to change: the archive path is
// assembled from the archive mount and --name, the data path from the volume
// mount and --path.
func (c *Cmd) checkPaths() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"--name", c.ArchivePath},
		{"--path", c.DataPath},
	} {
		if field.value == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}

		if err := shell.CheckSingleLine(field.name, field.value); err != nil {
			return err
		}
	}

	return nil
}

// compressProgram returns the command tar runs as its compressor, or the empty
// string when the archive is uncompressed.
func compressProgram(compression string, level int) (string, error) {
	switch compression {
	case CompressionNone:
		if level != 0 {
			return "", errors.New("compression level does not apply to " + CompressionNone)
		}

		return "", nil
	case CompressionZstd:
		if level == 0 {
			level = DefaultZstdLevel
		}

		if err := checkLevel(CompressionZstd, level, maxZstdLevel); err != nil {
			return "", err
		}

		return fmt.Sprintf("zstd -T%d -%d", compressThreads, level), nil
	case CompressionGzip:
		if level == 0 {
			level = DefaultGzipLevel
		}

		if err := checkLevel(CompressionGzip, level, maxGzipLevel); err != nil {
			return "", err
		}

		return fmt.Sprintf("pigz -p %d -%d", compressThreads, level), nil
	default:
		return "", unsupportedCompression(compression)
	}
}

// CheckLevel reports whether level is valid for the compression, so a caller
// can refuse a bad one before doing anything expensive.
func CheckLevel(compression string, level int) error {
	_, err := compressProgram(compression, level)

	return err
}

func checkLevel(compression string, level, maximum int) error {
	if level < minLevel || level > maximum {
		return fmt.Errorf("compression level %d is out of range for %s, must be between %d and %d",
			level, compression, minLevel, maximum)
	}

	return nil
}

func unsupportedCompression(compression string) error {
	return fmt.Errorf("unsupported compression: %q, must be one of %s",
		compression, strings.Join(Compressions(), ", "))
}

// StreamCmd holds the parameters for an archive that goes to or comes from an
// rclone remote as a stream, so it never lands on a disk in the pod.
//
// Unlike Cmd, this is a real pipe: tar on one side, rclone on the other. A
// pipe reports the exit status of its last command, so without pipefail a tar
// that died half way through would let rclone report a truncated object as a
// success. The line therefore starts by enabling it.
type StreamCmd struct {
	Direction   string
	RemotePath  string
	DataPath    string
	ConfigPath  string
	Compression string
	Level       int
	// ExtraArgs are raw rclone flags appended unquoted, the same
	// --rclone-extra-args the bucket workflow forwards. They land after the
	// computed --s3-chunk-size, so one given here overrides it.
	ExtraArgs string
	// Clean empties the data path before extracting, so that a restore
	// replaces the volume rather than merging into it. Restore only.
	Clean bool
}

// rcloneProgressFlags are the flags the job's log parser expects rclone to
// print its progress with.
const rcloneProgressFlags = "--stats 1s --stats-log-level NOTICE --use-json-log --stats-one-line"

// Build produces the full piped command string.
func (c *StreamCmd) Build() (string, error) {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"--archive-file", c.RemotePath},
		{"--path", c.DataPath},
		{"rclone config path", c.ConfigPath},
	} {
		if field.value == "" {
			return "", fmt.Errorf("%s must not be empty", field.name)
		}

		if err := shell.CheckSingleLine(field.name, field.value); err != nil {
			return "", err
		}
	}

	program, err := compressProgram(c.Compression, c.Level)
	if err != nil {
		return "", err
	}

	rclone := fmt.Sprintf("rclone --config %s %s", shell.Quote(c.ConfigPath), rcloneProgressFlags)

	switch c.Direction {
	case DirectionBackup:
		return c.buildBackup(program, rclone), nil
	case DirectionRestore:
		return c.buildRestore(rclone), nil
	default:
		return "", fmt.Errorf("invalid direction: %q, must be %q or %q",
			c.Direction, DirectionBackup, DirectionRestore)
	}
}

// Stream chunk sizing. An upload of unknown size cannot grow its chunk to
// fit, so the object is capped at chunk size times S3's part limit, 48 GiB at
// rclone's default. The job measures the volume's used bytes as it starts and
// sizes the chunk so the stream fits, assuming nothing compresses.
const (
	// streamPartBudget stays under S3's 10,000-part maximum, leaving room for
	// the estimate to be low.
	streamPartBudget = 9000
	// streamHeadroomNum and streamHeadroomDen scale the used bytes by 5/4 for
	// tar's own headers and anything written while the backup runs.
	streamHeadroomNum = 5
	streamHeadroomDen = 4
	// streamMinChunkMiB is S3's smallest allowed part, and rclone's default.
	streamMinChunkMiB = 5
)

// chunkSizing is the shell that sets $chunk to the part size in MiB. It reads
// df on the data path, which counts the whole filesystem and so bounds what
// tar can read even when --path names a subdirectory.
func chunkSizing(dataPath string) string {
	return fmt.Sprintf(
		`used=$(df -B1 %s | awk 'NR==2{print $3}'); `+
			`chunk=$(( (used * %d / %d / %d / 1048576) + 1 )); `+
			`[ "$chunk" -lt %d ] && chunk=%d; `,
		shell.Quote(dataPath), streamHeadroomNum, streamHeadroomDen, streamPartBudget,
		streamMinChunkMiB, streamMinChunkMiB)
}

// buildBackup pipes tar into rclone rcat. The computed chunk size goes before
// the user's extra args, so an explicit --s3-chunk-size there still wins: for a
// repeated flag rclone keeps the last one.
func (c *StreamCmd) buildBackup(program, rclone string) string {
	var builder strings.Builder

	builder.WriteString("set -o pipefail; ")
	builder.WriteString(chunkSizing(c.DataPath))
	builder.WriteString("tar -c --numeric-owner --xattrs --sparse " + lostFoundExclude)

	if program != "" {
		fmt.Fprintf(&builder, " -I %s", shell.Quote(program))
	}

	fmt.Fprintf(&builder, ` -f - -C %s . | %s rcat %s --s3-chunk-size "${chunk}M"`,
		shell.Quote(c.DataPath), rclone, shell.Quote(c.RemotePath))

	if c.ExtraArgs != "" {
		fmt.Fprintf(&builder, " %s", c.ExtraArgs)
	}

	return builder.String()
}

// buildRestore pipes rclone cat into tar. A pipe is not seekable, so tar
// cannot sniff the format the way it does from a file and has to be told the
// decompressor, to which it appends -d itself.
func (c *StreamCmd) buildRestore(rclone string) string {
	var builder strings.Builder

	fmt.Fprintf(&builder, "set -o pipefail; mkdir -p %s && ", shell.Quote(c.DataPath))

	if c.Clean {
		fmt.Fprintf(&builder, "%s && ", cleanCmd(c.DataPath))
	}

	fmt.Fprintf(&builder, "%s cat %s", rclone, shell.Quote(c.RemotePath))

	if c.ExtraArgs != "" {
		fmt.Fprintf(&builder, " %s", c.ExtraArgs)
	}

	builder.WriteString(" | tar -x --numeric-owner --xattrs " + lostFoundExclude)

	if decompressor := decompressProgram(c.Compression); decompressor != "" {
		fmt.Fprintf(&builder, " -I %s", shell.Quote(decompressor))
	}

	fmt.Fprintf(&builder, " -f - -C %s", shell.Quote(c.DataPath))

	return builder.String()
}

// decompressProgram names the program tar runs to read a compressed stream, or
// the empty string for an uncompressed one. tar adds -d itself.
func decompressProgram(compression string) string {
	switch compression {
	case CompressionZstd:
		return "zstd"
	case CompressionGzip:
		return fmt.Sprintf("pigz -p %d", compressThreads)
	default:
		return ""
	}
}
