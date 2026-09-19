package progress_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/utkuozdemir/pv-migrate/internal/archive/progress"
)

// zstdOutput is what the mover image actually prints, carriage returns and
// all. Captured from a real run rather than written from the manual, since the
// spacing is what the pattern has to survive.
const zstdOutput = "pv-migrate-bytes-total: 1073741824\n" +
	"\rRead:     0   B  ==>  0%" +
	"\rRead:  19.5 MiB  ==> 46%" +
	"\rRead:   512 MiB  ==> 36%" +
	"\r/*stdin*\\            : 36.16%   (   465 MiB =>    168 MiB, /*stdout*\\) \n"

func TestParserReadsTheCounter(t *testing.T) {
	t.Parallel()

	parse := progress.NewParser()

	_, err := parse("pv-migrate-bytes-total: 1073741824")
	require.ErrorIs(t, err, progress.ErrNoMatch, "the total is not itself progress")

	update, err := parse("Read:   512 MiB  ==> 36%")
	require.NoError(t, err)

	assert.Equal(t, int64(536870912), update.Transferred)
	assert.Equal(t, int64(1073741824), update.Total)
	assert.Equal(t, 50, update.Percentage)
}

// The percentage zstd prints is its compression ratio, not how far it has got.
// Reading it as progress would report a transfer stuck near a third forever.
func TestParserIgnoresTheRatio(t *testing.T) {
	t.Parallel()

	parse := progress.NewParser()

	_, err := parse("pv-migrate-bytes-total: 1048576")
	require.ErrorIs(t, err, progress.ErrNoMatch)

	update, err := parse("Read:   512 KiB  ==> 36%")
	require.NoError(t, err)

	assert.Equal(t, 50, update.Percentage, "half of a MiB is half, whatever it compressed to")
}

// Without the total there is a byte count and nothing to measure it against,
// which must not become a fabricated percentage.
func TestParserWithoutATotal(t *testing.T) {
	t.Parallel()

	parse := progress.NewParser()

	update, err := parse("Read:  19.5 MiB  ==> 46%")
	require.NoError(t, err)

	assert.Equal(t, int64(20447232), update.Transferred)
	assert.Equal(t, update.Transferred, update.Total,
		"a total no smaller than what has gone past, which the other parsers promise too")
	assert.Equal(t, 100, update.Percentage)
}

// The total is the volume's used bytes, an estimate of what tar will produce
// rather than a measurement, so the count can overtake it.
func TestParserCountCanOvertakeTheTotal(t *testing.T) {
	t.Parallel()

	parse := progress.NewParser()

	_, err := parse("pv-migrate-bytes-total: 1048576")
	require.ErrorIs(t, err, progress.ErrNoMatch)

	update, err := parse("Read:   4.0 MiB  ==> 36%")
	require.NoError(t, err)

	assert.Equal(t, int64(4194304), update.Total)
	assert.LessOrEqual(t, update.Percentage, 100, "a percentage stays inside its own range")
}

func TestParserRefusesEverythingElse(t *testing.T) {
	t.Parallel()

	parse := progress.NewParser()

	for _, line := range []string{
		"",
		"tar: ./x: Cannot open: Permission denied",
		`{"level":"info","stats":{"bytes":1}}`,
		"sending incremental file list",
		"Read:  19.5 ZiB  ==> 46%",
	} {
		_, err := parse(line)
		require.Error(t, err, "line %q is not progress", line)
	}
}

// FindLast is the path the status command takes, reading a whole log rather
// than following it.
func TestFindLastTakesTheFurthest(t *testing.T) {
	t.Parallel()

	update, ok := progress.FindLast(zstdOutput)
	require.True(t, ok)

	assert.Equal(t, int64(536870912), update.Transferred, "the last counter, not the first")
	assert.Equal(t, int64(1073741824), update.Total)
	assert.Equal(t, 50, update.Percentage)
}

func TestFindLastWithNoProgress(t *testing.T) {
	t.Parallel()

	_, ok := progress.FindLast("tar: ./x: Cannot open: Permission denied\n")
	assert.False(t, ok)
}

// The counter is overwritten in place with a carriage return, so one chunk of
// a log holds many updates and the reader has to split on it.
func TestOutputIsCarriageReturnSeparated(t *testing.T) {
	t.Parallel()

	assert.Greater(t, strings.Count(zstdOutput, "\r"), 1,
		"if zstd ever stops overwriting in place, the log splitter's CR handling matters less")
}
