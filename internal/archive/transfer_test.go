package archive_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/utkuozdemir/pv-migrate/internal/archive"
)

// fakeTools writes stand-ins for tar and ssh and returns the directory holding
// them.
//
// The tar stand-in writes a marker to stdout for a create and copies stdin to a
// file for an extract, so a run says whether the stream reached the extract at
// all. The ssh stand-in runs its last argument through a shell, which is what
// real ssh does with a remote command, and passes its own stdin along.
func fakeTools(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	const fakeTar = `#!/bin/sh
mode=$1
out=received
for arg in "$@"; do
  if [ "$prev" = "-C" ]; then dest=$arg; fi
  prev=$arg
done
case "$mode" in
  -c) printf '%s' "PAYLOAD" ;;
  -x) cat > "$dest/$out" ;;
esac
`

	const fakeSSH = `#!/bin/sh
for arg in "$@"; do last=$arg; done
exec /bin/sh -c "$last"
`

	//nolint:gosec // they have to be executable for the shell to find them
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tar"), []byte(fakeTar), 0o700))
	//nolint:gosec // as above
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ssh"), []byte(fakeSSH), 0o700))

	return dir
}

// runBuilt runs command with the stand-ins ahead of the real tools.
//
// bash rather than /bin/sh, which on many distributions is dash and has no
// `set -o pipefail`. The job container's shell is BusyBox ash, which does, and
// what this checks is shell grammar that the two agree on.
func runBuilt(t *testing.T, toolDir, command string) {
	t.Helper()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to stand in for the job container's ash")
	}

	shell := exec.CommandContext(t.Context(), bash, "-c", command)
	shell.Env = []string{"PATH=" + toolDir + ":/usr/bin:/bin"}

	out, err := shell.CombinedOutput()
	require.NoError(t, err, "the built command is not runnable: %s\n%s", command, out)
}

// TestTransferBuildStreamReachesTheExtract is the property the command exists
// for: whatever tar writes on one side has to arrive at the tar on the other.
//
// It runs the real shell rather than matching the string, because the way this
// breaks is shell precedence, not a missing word. A pipe binds tighter than
// &&, so "ssh ... | mkdir -p d && tar -x" reads as "(ssh | mkdir) && tar -x":
// every argument is present and correct, mkdir throws the stream away, and tar
// extracts nothing from an empty stdin.
func TestTransferBuildStreamReachesTheExtract(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the built command is only ever run by the Linux job container's shell")
	}

	for name, push := range map[string]bool{"pull": false, "push": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tools := fakeTools(t)
			dest := t.TempDir()

			cmd := archive.TransferCmd{
				Push: push, SSHUser: "root", SSHHost: "sshd.ns",
				SrcPath: t.TempDir(), DestPath: dest,
			}

			built, err := cmd.Build()
			require.NoError(t, err)

			runBuilt(t, tools, built)

			got, err := os.ReadFile(filepath.Join(dest, "received"))
			require.NoError(t, err, "the extract side wrote nothing, so the stream never reached it")
			assert.Equal(t, "PAYLOAD", string(got))
		})
	}
}

// The mount strategy holds both volumes in one pod, so the copy is a pipe to
// itself. It still has to arrive, and it must not pay for compression that
// nothing crosses a network to benefit from.
func TestTransferBuildLocal(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the built command is only ever run by the Linux job container's shell")
	}

	tools := fakeTools(t)
	dest := t.TempDir()

	cmd := archive.TransferCmd{Local: true, SrcPath: t.TempDir(), DestPath: dest}

	built, err := cmd.Build()
	require.NoError(t, err)

	assert.NotContains(t, built, "ssh ", "there is no remote side to reach")
	assert.NotContains(t, built, "zstd", "compressing a pipe to itself only costs")

	runBuilt(t, tools, built)

	got, err := os.ReadFile(filepath.Join(dest, "received"))
	require.NoError(t, err, "the extract side wrote nothing, so the stream never reached it")
	assert.Equal(t, "PAYLOAD", string(got))
}

// The destination is emptied before the extract, and only when asked.
func TestTransferBuildClean(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the built command is only ever run by the Linux job container's shell")
	}

	tools := fakeTools(t)
	dest := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dest, "stale"), []byte("old"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dest, "lost+found"), 0o700))

	cmd := archive.TransferCmd{
		SSHUser: "root", SSHHost: "sshd.ns",
		SrcPath: t.TempDir(), DestPath: dest, Clean: true,
	}

	built, err := cmd.Build()
	require.NoError(t, err)

	runBuilt(t, tools, built)

	_, err = os.Stat(filepath.Join(dest, "stale"))
	assert.True(t, os.IsNotExist(err), "a file the archive does not carry must not survive a clean")

	_, err = os.Stat(filepath.Join(dest, "lost+found"))
	require.NoError(t, err, "the filesystem's own recovery directory is not the copy's to remove")
}

// The trailing slash decides whether the source directory itself travels or
// only its contents, and the two movers have to agree on it.
//
// rsync reads "dir" as the directory and "dir/" as its contents, and the path
// resolver preserves that spelling for it. tar draws the same distinction
// through the member it is given, so archiving "." regardless would drop a
// directory level for anyone who wrote --source-path without a slash. Checked
// against real rsync and real tar: "-C /src data" matches "rsync /src/data",
// and "-C /src/data ." matches "rsync /src/data/".
func TestTransferBuildKeepsTheTrailingSlashMeaning(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ src, want string }{
		"the directory itself": {"/source/data", "-C '/source' 'data'"},
		"its contents":         {"/source/data/", "-C '/source/data/' '.'"},
		"the whole volume":     {"/source/", "-C '/source/' '.'"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := archive.TransferCmd{Local: true, SrcPath: tc.src, DestPath: "/dest/"}

			built, err := cmd.Build()
			require.NoError(t, err)

			assert.Contains(t, built, tc.want)
		})
	}
}

// Transport compresses by default and at the cheapest level, since the stream
// is discarded on arrival and the only question is whether compressing costs
// less than sending the bytes.
func TestTransferBuildCompressesByDefault(t *testing.T) {
	t.Parallel()

	cmd := archive.TransferCmd{
		SSHUser: "root", SSHHost: "sshd.ns", SrcPath: "/source", DestPath: "/dest",
	}

	built, err := cmd.Build()
	require.NoError(t, err)

	assert.Contains(t, built, "zstd -T4 -1")
	assert.Contains(t, built, "set -o pipefail",
		"a pipeline reports only its last command, so a tar that died half way "+
			"through would otherwise read as a copy that finished")
}

// An empty path or host would produce a command that runs and does the wrong
// thing, so it is refused while the flag name is still known.
func TestTransferBuildRejectsEmptyFields(t *testing.T) {
	t.Parallel()

	full := archive.TransferCmd{
		SSHUser: "root", SSHHost: "sshd.ns", SrcPath: "/source", DestPath: "/dest",
	}

	for name, blank := range map[string]func(*archive.TransferCmd){
		"--source-path": func(c *archive.TransferCmd) { c.SrcPath = "" },
		"--dest-path":   func(c *archive.TransferCmd) { c.DestPath = "" },
		"ssh user":      func(c *archive.TransferCmd) { c.SSHUser = "" },
		"ssh host":      func(c *archive.TransferCmd) { c.SSHHost = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := full
			blank(&cmd)

			_, err := cmd.Build()
			require.ErrorContains(t, err, name)
		})
	}
}

// A path carrying a character the chart's YAML cannot hold on one line is
// refused rather than quoted, so the error names the flag instead of a
// template line.
func TestTransferBuildRejectsUnrenderablePaths(t *testing.T) {
	t.Parallel()

	cmd := archive.TransferCmd{
		SSHUser: "root", SSHHost: "sshd.ns",
		SrcPath: "/source\nrm -rf /", DestPath: "/dest",
	}

	_, err := cmd.Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--source-path")
}

// Quoting is the other way this breaks silently: a path with a space has to
// reach tar as one argument.
func TestTransferBuildQuotesPaths(t *testing.T) {
	t.Parallel()

	// Each path is checked on the side that runs locally. The other one sits
	// inside the remote command, which is itself one quoted word, so it is
	// quoted twice over and does not read back as a plain single-quoted path.
	push := archive.TransferCmd{
		Push: true, SSHUser: "root", SSHHost: "sshd.ns",
		SrcPath: "/source dir/", DestPath: "/dest dir/",
	}

	built, err := push.Build()
	require.NoError(t, err)
	assert.Contains(t, built, "-C '/source dir/'")

	pull := push
	pull.Push = false

	built, err = pull.Build()
	require.NoError(t, err)
	assert.Contains(t, built, "-C '/dest dir/'")
}
