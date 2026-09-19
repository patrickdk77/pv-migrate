package jobprogress

import (
	"strings"

	archiveprogress "github.com/utkuozdemir/pv-migrate/internal/archive/progress"
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
		// Which program is in this job depends on --mover, and the job's name
		// does not say. Both parsers are tried rather than plumbing the choice
		// down here: they read formats nothing else produces, so whichever
		// matches is the one that ran.
		options.ParseLineFunc = firstMatch(rsyncprogress.ParseLine, archiveprogress.NewParser())
		options.Source = "rsync"
	case strings.HasSuffix(jobName, rcloneSuffix):
		options.ParseLineFunc = rcloneprogress.ParseLine
		options.Source = "rclone"
	case strings.HasSuffix(jobName, tarSuffix):
		// A tar job streaming to S3 prints rclone's stats; one writing to a
		// volume prints zstd's counter.
		options.ParseLineFunc = firstMatch(rcloneprogress.ParseLine, archiveprogress.NewParser())
		options.Source = "tar"
	}

	return progresslog.NewLogger(options)
}

func FindLast(jobName, text string) (progresslog.Update, bool) {
	switch {
	case strings.HasSuffix(jobName, rsyncSuffix):
		if update, ok := archiveprogress.FindLast(text); ok {
			return update, true
		}

		return rsyncprogress.FindLast(text), true
	case strings.HasSuffix(jobName, rcloneSuffix), strings.HasSuffix(jobName, tarSuffix):
		if update, ok := archiveprogress.FindLast(text); ok {
			return update, true
		}

		return rcloneprogress.FindLast(text), true
	default:
		return progresslog.Update{}, false
	}
}

// firstMatch returns a parser that hands a line to each of its own in turn and
// reports the first that reads it.
//
// Safe because the formats do not overlap: rsync's progress line, rclone's
// JSON stats and zstd's counter each fail to parse as either of the others, so
// at most one can match and the order between them does not decide anything.
func firstMatch(parsers ...progresslog.ParseLineFunc) progresslog.ParseLineFunc {
	return func(line string) (progresslog.Update, error) {
		var err error

		for _, parse := range parsers {
			update, parseErr := parse(line)
			if parseErr == nil {
				return update, nil
			}

			err = parseErr
		}

		return progresslog.Update{}, err
	}
}
