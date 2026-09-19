package archive

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/utkuozdemir/pv-migrate/internal/archive/progress"
	"github.com/utkuozdemir/pv-migrate/internal/shell"
)

// TransportLevel is the compression level used for a stream that is thrown
// away once it arrives.
//
// One, not the stored archive's default: this compresses to spend less time on
// the wire and nothing keeps the result, so the only question is whether
// compressing costs less than sending the bytes. At level 1 it always does.
const TransportLevel = 1

// sshOptions are the options every ssh invocation carries.
//
// They mirror what internal/rsync passes, deliberately rather than by sharing
// a constant: the two movers are independent and rsync's transport is not this
// package's to change. ServerAliveInterval and ServerAliveCountMax keep an
// intermediate load balancer or proxy from dropping the session while tar is
// walking a large tree and sending nothing.
var sshOptions = []string{
	"-o", "StrictHostKeyChecking=no",
	"-o", "UserKnownHostsFile=/dev/null",
	"-o", "ConnectTimeout=5",
	"-o", "ServerAliveInterval=10",
	"-o", "ServerAliveCountMax=3",
}

// TransferCmd builds the command that copies one volume's contents into
// another over ssh, with tar at both ends.
//
// tar rather than rsync because tar carries what rsync's archive mode leaves
// behind: hard links, sparse regions and extended attributes. The cost is that
// every run sends the whole tree, since tar has no notion of what the
// destination already holds, which makes this the option for a faithful copy
// rather than for catching a destination up.
type TransferCmd struct {
	// Push reverses which side reads. False runs tar -c on the remote and
	// extracts locally, true reads locally and extracts on the remote.
	Push        bool
	SSHUser     string
	SSHHost     string
	Port        int
	SrcPath     string
	DestPath    string
	Compression string
	Level       int
	// Clean empties the destination before extracting, so the copy replaces
	// what is there rather than merging into it.
	Clean bool
	// Local runs both ends in one pod with no ssh between them. Compression
	// is skipped then whatever Compression says, because nothing crosses a
	// network and compressing a pipe to itself only costs.
	Local bool
}

// Build produces the full command string.
func (c *TransferCmd) Build() (string, error) {
	if err := c.check(); err != nil {
		return "", err
	}

	compression := c.Compression
	if compression == "" {
		compression = CompressionZstd
	}

	if c.Local {
		compression = CompressionNone
	}

	level := c.Level
	if compression == CompressionNone {
		level = 0
	} else if level == 0 {
		level = TransportLevel
	}

	program, err := compressProgram(compression, level)
	if err != nil {
		return "", err
	}

	// zstd writes its counter to stderr and suppresses it unless stderr is a
	// terminal, which in a job pod it never is. Only zstd: transport never
	// selects another compressor, and pigz has no equivalent.
	if compression == CompressionZstd {
		program += " --progress"
	}

	create := tarCreate(program, c.SrcPath)
	prepare := preparePath(c.DestPath, c.Clean)
	extract := tarExtract(decompressProgram(compression), c.DestPath)

	// pipefail on every form: a pipeline reports only its last command, so
	// without it a tar that died half way through would be read as a copy
	// that finished.
	if c.Local {
		return fmt.Sprintf("set -o pipefail; %s%s | %s", prepare, create, extract), nil
	}

	if c.Push {
		// The remote shell runs the whole right side, so its own setup goes
		// inside the quoted command.
		return fmt.Sprintf("set -o pipefail; %s | %s %s",
			create, c.sshPrefix(), shell.Quote(prepare+extract)), nil
	}

	// The setup runs before the pipeline rather than beside the extract. A
	// pipe binds tighter than &&, so "ssh ... | mkdir -p d && tar -x" feeds
	// the stream to mkdir, which reads nothing, and leaves tar with no input.
	return fmt.Sprintf("set -o pipefail; %s%s %s | %s",
		prepare, c.sshPrefix(), shell.Quote(create), extract), nil
}

// preparePath makes the destination and, when asked, empties it. It ends with
// "&& " so the caller can put it in front of whatever must not run if it
// fails.
func preparePath(path string, clean bool) string {
	prepare := fmt.Sprintf("mkdir -p %s && ", shell.Quote(path))

	if clean {
		prepare += cleanCmd(path) + " && "
	}

	return prepare
}

// tarCreate writes the tree at path to stdout, after announcing how much there
// is to read.
//
// The announcement is the only figure available for a total. zstd reports what
// it has read, but a stream has no length it could know ahead of time, so
// without this there is a byte count and nothing to measure it against. The
// volume's used bytes is an estimate of what tar will produce rather than a
// measurement of it, which is why the parser lets the count overtake it.
func tarCreate(program, srcPath string) string {
	var builder strings.Builder

	fmt.Fprintf(&builder, `printf '%s %%s\n' "$(df -B1 %s | awk 'NR==2{print $3}')" >&2; `,
		progress.TotalMarker, shell.Quote(srcPath))

	builder.WriteString("tar -c --numeric-owner --xattrs --sparse " + lostFoundExclude)

	if program != "" {
		fmt.Fprintf(&builder, " -I %s", shell.Quote(program))
	}

	dir, member := tarSource(srcPath)
	fmt.Fprintf(&builder, " -f - -C %s %s", shell.Quote(dir), shell.Quote(member))

	return builder.String()
}

// tarSource splits a resolved source path into the directory tar changes to
// and the member it archives.
//
// The trailing slash carries the caller's intent, which the path resolver
// preserves for rsync's sake: "dir" means the directory itself and "dir/"
// means its contents. tar draws the same distinction, but through which
// member it is given rather than through the path, so archiving "." whatever
// the path said would quietly drop a directory level for everyone who wrote
// --source-path without a slash.
func tarSource(srcPath string) (string, string) {
	if strings.HasSuffix(srcPath, "/") {
		return srcPath, "."
	}

	return path.Dir(srcPath), path.Base(srcPath)
}

// tarExtract reads a tree from stdin into path.
//
// The decompressor is named rather than sniffed because a pipe is not
// seekable, so tar cannot read the format back from the stream the way it does
// from a file.
func tarExtract(decompressor, path string) string {
	var builder strings.Builder

	builder.WriteString("tar -x --numeric-owner --xattrs " + lostFoundExclude)

	if decompressor != "" {
		fmt.Fprintf(&builder, " -I %s", shell.Quote(decompressor))
	}

	fmt.Fprintf(&builder, " -f - -C %s", shell.Quote(path))

	return builder.String()
}

// sshPrefix is ssh and its options, up to and including the target.
func (c *TransferCmd) sshPrefix() string {
	args := []string{"ssh"}
	args = append(args, sshOptions...)

	if c.Port != 0 {
		args = append(args, "-p", strconv.Itoa(c.Port))
	}

	return strings.Join(args, " ") + " " + shell.Quote(c.SSHUser+"@"+c.SSHHost)
}

// check rejects what the built command could not carry.
func (c *TransferCmd) check() error {
	type field struct {
		name  string
		value string
	}

	fields := []field{
		{"--source-path", c.SrcPath},
		{"--dest-path", c.DestPath},
	}

	if !c.Local {
		fields = append(fields,
			field{"ssh user", c.SSHUser},
			field{"ssh host", c.SSHHost},
		)
	}

	for _, field := range fields {
		if field.value == "" {
			return fmt.Errorf("%s must not be empty", field.name)
		}

		if err := shell.CheckSingleLine(field.name, field.value); err != nil {
			return err
		}
	}

	return nil
}
