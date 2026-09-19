package jobprogress

import (
	"strings"

	"github.com/utkuozdemir/pv-migrate/internal/progresslog"
	rcloneprogress "github.com/utkuozdemir/pv-migrate/internal/rclone/progress"
	rsyncprogress "github.com/utkuozdemir/pv-migrate/internal/rsync/progress"
)

const (
	rsyncSuffix  = "-rsync"
	rcloneSuffix = "-rclone"
	tarSuffix    = "-tar"
)

func Description(jobName string) string {
	switch {
	case strings.HasSuffix(jobName, rsyncSuffix):
		return "rsync"
	case strings.HasSuffix(jobName, rcloneSuffix):
		return "rclone"
	case strings.HasSuffix(jobName, tarSuffix):
		return "tar"
	default:
		return "job"
	}
}

func NewLogger(jobName string, options progresslog.LoggerOptions) *progresslog.Logger {
	switch {
	case strings.HasSuffix(jobName, rsyncSuffix):
		options.ParseLineFunc = rsyncprogress.ParseLine
		options.Source = "rsync"
	case strings.HasSuffix(jobName, rcloneSuffix):
		options.ParseLineFunc = rcloneprogress.ParseLine
		options.Source = "rclone"
	case strings.HasSuffix(jobName, tarSuffix):
		// A tar job streaming to S3 prints rclone's stats; one writing to a
		// volume prints nothing, and finds nothing.
		options.ParseLineFunc = rcloneprogress.ParseLine
		options.Source = "tar"
	}

	return progresslog.NewLogger(options)
}

func FindLast(jobName, text string) (progresslog.Update, bool) {
	switch {
	case strings.HasSuffix(jobName, rsyncSuffix):
		return rsyncprogress.FindLast(text), true
	case strings.HasSuffix(jobName, rcloneSuffix), strings.HasSuffix(jobName, tarSuffix):
		return rcloneprogress.FindLast(text), true
	default:
		return progresslog.Update{}, false
	}
}
