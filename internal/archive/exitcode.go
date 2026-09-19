package archive

// ChangedFilesExitCode is what tar exits with when files changed while it was
// reading them. What it did archive is intact.
const ChangedFilesExitCode = 1

// ToleratedMarker is the line the job script prints when it treats such an
// exit as a success. The client scans the log for it, since on a job that
// succeeds it never sees the raw output.
const ToleratedMarker = "pv-migrate: some files changed while tar was reading them"

// ToleratedExitCodes are the codes that count as a success with caveats. tar's
// 1 is the counterpart of rsync's 24: ordinary on a volume something is still
// writing to.
//
// They are tar's codes rather than any other mover's. Read against rsync's
// table the same number is a deterministic usage error, which would fail a
// backup that in fact succeeded.
func ToleratedExitCodes() []int { return []int{ChangedFilesExitCode} }

// NoRetryExitCodes is empty: tar reports every fatal condition as 2, from a
// path it could not open to a truncated stream, and some of those clear on
// their own. Nothing here is deterministic enough to stop for.
func NoRetryExitCodes() []int { return nil }

// Interpret gives the meaning GNU tar attaches to an exit code, or the empty
// string for one it does not document. For an archive streamed through
// rclone the code is whichever side of the pipe failed first, so a code here
// may also be rclone's; the job's own log says which.
func Interpret(code int) string {
	switch code {
	case 0:
		return ""
	case ChangedFilesExitCode:
		return "some files changed while tar was reading them, or differ from the archive"
	case 2:
		return "tar hit a fatal error, such as a path it could not open or a truncated stream"
	default:
		return ""
	}
}
