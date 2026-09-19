package strategy

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/utkuozdemir/pv-migrate/internal/archive"
	"github.com/utkuozdemir/pv-migrate/internal/migration"
	"github.com/utkuozdemir/pv-migrate/internal/rsync"
)

// moverCommand is what the job runs, together with the exit-code policy that
// goes with it.
//
// The policy travels with the command because it belongs to the program being
// run: rsync's 24 means vanished source files and is a success with caveats,
// while tar's 24 means nothing at all and its 1 is the condition rsync calls a
// usage error. One table read against the other mover's codes turns a
// successful transfer into a failure, or the reverse.
type moverCommand struct {
	command string
	policy  moverPolicy
}

// moverPolicy is one mover's reading of an exit status. It is consulted both
// by the job script, through the chart values below, and directly in Go by the
// local strategy, which runs the command over an ssh session and has no script
// to apply it.
type moverPolicy struct {
	tolerated []int
	noRetry   []int
	marker    string
	interpret func(int) string
}

// tolerates reports whether code is a success with caveats for this mover.
func (p moverPolicy) tolerates(code int) bool {
	return slices.Contains(p.tolerated, code)
}

// values returns the chart values carrying the command and its policy.
func (m moverCommand) values() map[string]any {
	return map[string]any{
		"command":            m.command,
		"toleratedExitCodes": shellPattern(m.policy.tolerated),
		"toleratedMarker":    m.policy.marker,
		"noRetryExitCodes":   shellPattern(m.policy.noRetry),
	}
}

// shellPattern renders exit codes as a shell case pattern. An empty set gives
// an empty string, which the template reads as "no such codes" and leaves the
// whole branch out rather than emitting a case with nothing to match.
func shellPattern(codes []int) string {
	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		parts = append(parts, strconv.Itoa(code))
	}

	return strings.Join(parts, "|")
}

func rsyncPolicy() moverPolicy {
	return moverPolicy{
		tolerated: rsync.ToleratedExitCodes(),
		noRetry:   rsync.NoRetryExitCodes(),
		marker:    rsync.VanishedFilesMarker + "; treated as success",
		interpret: rsync.Interpret,
	}
}

func tarPolicy() moverPolicy {
	return moverPolicy{
		tolerated: archive.ToleratedExitCodes(),
		noRetry:   archive.NoRetryExitCodes(),
		marker:    archive.ToleratedMarker + "; treated as success",
		interpret: archive.Interpret,
	}
}

func rsyncMover(command string) moverCommand {
	return moverCommand{command: command, policy: rsyncPolicy()}
}

func tarMover(command string) moverCommand {
	return moverCommand{command: command, policy: tarPolicy()}
}

// moverKind resolves which mover the request asked for, and refuses a name
// that is neither. Falling back to a default here would silently run a
// different program than the one that was asked for.
func moverKind(req *migration.Request) (string, error) {
	switch req.Mover {
	case "", migration.MoverRsync:
		return migration.MoverRsync, nil
	case migration.MoverTar:
		return migration.MoverTar, nil
	default:
		return "", fmt.Errorf("unknown --mover %q, expected one of: %s",
			req.Mover, strings.Join(migration.Movers(), ", "))
	}
}

// CheckMoverFlags refuses a request whose mover and flags disagree, before any
// strategy is tried.
//
// A strategy that declines is ordinary: the ladder moves on and only an
// exhausted ladder is an error, which collapses every reason into "no strategy
// could complete the migration". A flag conflict is deterministic and true of
// every strategy, so discovering it there loses the one sentence that says
// what to change.
func CheckMoverFlags(req *migration.Request) error {
	kind, err := moverKind(req)
	if err != nil {
		return err
	}

	if kind != migration.MoverTar {
		return nil
	}

	return checkTarFlags(req)
}

// checkTarFlags refuses the flags that only describe rsync.
func checkTarFlags(req *migration.Request) error {
	if req.RsyncExtraArgs != "" {
		return fmt.Errorf(
			"--rsync-extra-args names raw rsync flags, and --mover %s runs tar instead",
			migration.MoverTar)
	}

	return nil
}

// tarCompression maps the request onto what the transfer should compress with.
// Compression is on for anything crossing a network, since the stream is
// discarded on arrival and at level 1 compressing always costs less than
// sending the bytes.
func tarCompression(req *migration.Request) string {
	if req.NoCompress {
		return archive.CompressionNone
	}

	return archive.CompressionZstd
}

// buildMoverCmdSSH builds the command for a transfer that reaches the other
// volume through the sshd pod.
func buildMoverCmdSSH(
	req *migration.Request, push bool, sshHost string, port int,
) (moverCommand, error) {
	kind, err := moverKind(req)
	if err != nil {
		return moverCommand{}, err
	}

	if kind != migration.MoverTar {
		built, err := buildRsyncCmdString(req, push, sshHost, port)
		if err != nil {
			return moverCommand{}, err
		}

		return rsyncMover(built), nil
	}

	if err := checkTarFlags(req); err != nil {
		return moverCommand{}, err
	}

	srcPath, destPath, err := resolveMountPaths(req)
	if err != nil {
		return moverCommand{}, err
	}

	cmd := archive.TransferCmd{
		Push:        push,
		SSHUser:     sshUser(req),
		SSHHost:     sshHost,
		Port:        port,
		SrcPath:     srcPath,
		DestPath:    destPath,
		Compression: tarCompression(req),
		Clean:       req.DeleteExtraneousFiles,
	}

	built, err := cmd.Build()
	if err != nil {
		return moverCommand{}, fmt.Errorf("failed to build tar command: %w", err)
	}

	return tarMover(built), nil
}

// buildMoverCmdLocal builds the command for a transfer with both volumes
// mounted in the one pod.
func buildMoverCmdLocal(mig *migration.Migration) (moverCommand, error) {
	req := mig.Request

	kind, err := moverKind(req)
	if err != nil {
		return moverCommand{}, err
	}

	if kind != migration.MoverTar {
		built, err := buildRsyncCmdMount(mig)
		if err != nil {
			return moverCommand{}, err
		}

		return rsyncMover(built), nil
	}

	if err := checkTarFlags(req); err != nil {
		return moverCommand{}, err
	}

	srcPath, destPath, err := resolveMountPaths(req)
	if err != nil {
		return moverCommand{}, err
	}

	cmd := archive.TransferCmd{
		Local:    true,
		SrcPath:  srcPath,
		DestPath: destPath,
		Clean:    req.DeleteExtraneousFiles,
	}

	built, err := cmd.Build()
	if err != nil {
		return moverCommand{}, fmt.Errorf("failed to build tar command: %w", err)
	}

	return tarMover(built), nil
}

// buildMoverCmdSSHSession builds the command the local strategy runs over its
// own ssh session, which reaches the destination back through the reverse
// tunnel on localhost.
func buildMoverCmdSSHSession(mig *migration.Migration) (moverCommand, error) {
	req := mig.Request

	kind, err := moverKind(req)
	if err != nil {
		return moverCommand{}, err
	}

	if kind != migration.MoverTar {
		built, err := buildRsyncCmdLocal(mig)
		if err != nil {
			return moverCommand{}, err
		}

		return rsyncMover(built), nil
	}

	if err := checkTarFlags(req); err != nil {
		return moverCommand{}, err
	}

	srcPath, destPath, err := resolveMountPaths(req)
	if err != nil {
		return moverCommand{}, err
	}

	cmd := archive.TransferCmd{
		Push:        true,
		SSHUser:     sshUser(req),
		SSHHost:     "localhost",
		Port:        req.SSHReverseTunnelPort,
		SrcPath:     srcPath,
		DestPath:    destPath,
		Compression: tarCompression(req),
		Clean:       req.DeleteExtraneousFiles,
	}

	built, err := cmd.Build()
	if err != nil {
		return moverCommand{}, fmt.Errorf("failed to build tar command: %w", err)
	}

	return tarMover(built), nil
}
