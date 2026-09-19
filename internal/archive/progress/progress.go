// Package progress reads the counter zstd prints while it compresses a
// stream, which is the only thing a tar pipe says about how far it has got.
//
// tar itself prints nothing a parser can use. zstd does, but only when told
// to: it writes the counter to stderr and suppresses it unless stderr is a
// terminal, which in a job pod it never is, so the command passes --progress.
package progress

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/utkuozdemir/pv-migrate/internal/progresslog"
)

// TotalMarker introduces the line the command prints before it starts, giving
// the number of bytes the volume holds.
//
// zstd cannot supply it. It reports what it has read and what that compressed
// to, but a stream has no length it could know in advance, so the percentage
// it prints is the compression ratio rather than progress. Without this line
// there is a byte count and no way to say how far through it is.
const TotalMarker = "pv-migrate-bytes-total:"

// readLine matches zstd's counter, for example "Read:  41.5 MiB  ==> 37%".
// The trailing percentage is the ratio and is deliberately not captured.
var readLine = regexp.MustCompile(`Read:\s+([0-9.]+)\s*([KMGT]?i?B)\b`)

var totalLine = regexp.MustCompile(regexp.QuoteMeta(TotalMarker) + `\s*(\d+)`)

// ErrNoMatch reports a line that carries no progress, which is most of them.
var ErrNoMatch = errors.New("line carries no progress")

// units are the suffixes zstd prints, and what each is worth in bytes.
var units = map[string]int64{
	"B":   1,
	"KiB": 1 << 10,
	"MiB": 1 << 20,
	"GiB": 1 << 30,
	"TiB": 1 << 40,
}

// NewParser returns a parser that remembers the total once it has seen it.
//
// The total arrives on its own line, before any counter, so a parser that
// forgot it between lines could never report a percentage. Each stream gets
// its own parser, and a retried stream that reprints the marker simply
// overwrites it with the same value.
func NewParser() progresslog.ParseLineFunc {
	var total int64

	return func(line string) (progresslog.Update, error) {
		if match := totalLine.FindStringSubmatch(line); match != nil {
			parsed, err := strconv.ParseInt(match[1], 10, 64)
			if err != nil {
				return progresslog.Update{}, fmt.Errorf("unreadable total %q: %w", match[1], err)
			}

			total = parsed

			return progresslog.Update{}, ErrNoMatch
		}

		return parseRead(line, total)
	}
}

// parseRead turns one counter line into an update.
func parseRead(line string, total int64) (progresslog.Update, error) {
	match := readLine.FindStringSubmatch(line)
	if match == nil {
		return progresslog.Update{}, ErrNoMatch
	}

	scale, ok := units[match[2]]
	if !ok {
		return progresslog.Update{}, fmt.Errorf("%w: unknown unit %q", ErrNoMatch, match[2])
	}

	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return progresslog.Update{}, fmt.Errorf("unreadable byte count %q: %w", match[1], err)
	}

	if value < 0 {
		return progresslog.Update{}, fmt.Errorf("%w: negative byte count %q", ErrNoMatch, match[1])
	}

	transferred := int64(value * float64(scale))

	// The same guarantee the other parsers make: a total never smaller than
	// what has already gone past, and a percentage inside its own range. The
	// total is the volume's used bytes, which is an estimate of what tar will
	// produce rather than a measurement of it, so it can be overtaken.
	if total < transferred {
		total = transferred
	}

	percentage := 0
	if total > 0 {
		percentage = int(transferred * 100 / total) //nolint:mnd
	}

	return progresslog.Update{
		Line:        strings.TrimSpace(line),
		Percentage:  percentage,
		Transferred: transferred,
		Total:       total,
	}, nil
}

// FindLast returns the furthest progress in a block of text, for a caller that
// reads a job's whole log rather than following it.
func FindLast(text string) (progresslog.Update, bool) {
	var total int64

	if match := totalLine.FindStringSubmatch(text); match != nil {
		total, _ = strconv.ParseInt(match[1], 10, 64)
	}

	matches := readLine.FindAllString(text, -1)
	for _, match := range slices.Backward(matches) {
		update, err := parseRead(match, total)
		if err == nil {
			return update, true
		}
	}

	return progresslog.Update{}, false
}
