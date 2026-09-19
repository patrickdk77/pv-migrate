package archive

// Interpret gives the meaning GNU tar attaches to an exit code, or the empty
// string for one it does not document. For an archive streamed through
// rclone the code is whichever side of the pipe failed first, so a code here
// may also be rclone's; the job's own log says which.
func Interpret(code int) string {
	switch code {
	case 0:
		return ""
	case 1:
		return "some files changed while tar was reading them, or differ from the archive"
	case 2:
		return "tar hit a fatal error, such as a path it could not open or a truncated stream"
	default:
		return ""
	}
}
