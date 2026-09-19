package flush_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/utkuozdemir/pv-migrate/internal/flush"
)

func TestLookup_Known(t *testing.T) {
	t.Parallel()

	spec, err := flush.Lookup("mysql")
	require.NoError(t, err)
	assert.Equal(t, "LOCK INSTANCE FOR BACKUP;", spec.Lock)
	assert.Equal(t, "UNLOCK INSTANCE;", spec.Unlock)
}

func TestLookup_Unknown(t *testing.T) {
	t.Parallel()

	_, err := flush.Lookup("oracle")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported --flush "oracle"`)
	assert.Contains(t, err.Error(), "mysql")
}

func TestKinds_SortedAndNonEmpty(t *testing.T) {
	t.Parallel()

	kinds := flush.Kinds()
	require.NotEmpty(t, kinds)
	assert.True(t, sort.StringsAreSorted(kinds), "help text should list kinds in a stable order")
}

// Every kind has to be a complete recipe for its mode. A session kind needs
// its statements and a probe that prints the marker, or the lock can never be
// confirmed; a pair kind needs a release and a check that it took.
func TestEverySpecIsComplete(t *testing.T) {
	t.Parallel()

	for _, kind := range flush.Kinds() {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			spec, err := flush.Lookup(kind)
			require.NoError(t, err)

			assert.NotEmpty(t, spec.Command, "command")
			assert.NotEmpty(t, spec.Note, "note")

			switch spec.Mode {
			case flush.ModeSession:
				assert.NotEmpty(t, spec.Lock, "lock")
				assert.NotEmpty(t, spec.Probe, "probe")
				assert.NotEmpty(t, spec.Marker, "marker")
				assert.NotEmpty(t, spec.Unlock, "unlock")
				assert.Contains(t, spec.Probe, spec.Marker,
					"the probe must print the marker the session waits for")
				assert.Empty(t, spec.Release, "a session kind releases over stdin, not a command")
			case flush.ModeOneShot:
				assert.Empty(t, spec.Release, "a one-shot kind holds nothing to release")
				assert.Empty(t, spec.Lock, "a one-shot kind sends nothing over stdin")
			case flush.ModePair:
				assert.NotEmpty(t, spec.Release, "release")
				assert.NotEmpty(t, spec.ReleaseMarker, "release marker")
				// The release must be able to print its own marker, or the
				// check can never fire and a leaked lock passes as success.
				prefix, _, found := strings.Cut(spec.ReleaseMarker, "=")
				assert.True(t, found, "a release marker should pin a value, not just a word")
				assert.Contains(t, strings.Join(spec.Release, " "), prefix,
					"the release command must print its own marker")
			default:
				t.Fatalf("unknown mode %d", spec.Mode)
			}
		})
	}
}

func TestKinds_CoversEveryResearchedDatabase(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"mariadb", "mongodb", "mysql", "postgres", "scylladb"}, flush.Kinds())
}

func TestModes(t *testing.T) {
	t.Parallel()

	for kind, want := range map[string]flush.Mode{
		"mysql":    flush.ModeSession,
		"mariadb":  flush.ModeSession,
		"postgres": flush.ModeOneShot,
		"scylladb": flush.ModeOneShot,
		"mongodb":  flush.ModePair,
	} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			spec, err := flush.Lookup(kind)
			require.NoError(t, err)
			assert.Equal(t, want, spec.Mode)
		})
	}
}

// A session kind holds the client's stdin open across the snapshot, and both
// the MySQL and MariaDB clients block-buffer stdout into a pipe. Without
// --unbuffered the probe's marker stays in the client's buffer until the
// session ends, so the lock is taken but never confirmed and every run times
// out. Verified against mysql 8.4 and mariadb 11.4.
func TestSessionClientsAreUnbuffered(t *testing.T) {
	t.Parallel()

	for _, kind := range flush.Kinds() {
		spec, err := flush.Lookup(kind)
		require.NoError(t, err)

		if spec.Mode != flush.ModeSession {
			continue
		}

		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			assert.Contains(t, strings.Join(spec.Command, " "), "--unbuffered",
				"a held-session client must flush after each statement")
		})
	}
}
