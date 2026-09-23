package flush_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/utkuozdemir/pv-migrate/internal/flush"
)

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

// Every kind has to be a complete recipe for its mode. A session kind has to
// ask the server what it is and have a probe that prints the marker, or the
// lock can never be chosen or confirmed; a pair kind needs a release and a
// check that it took.
func TestEverySpecIsComplete(t *testing.T) {
	t.Parallel()

	for _, kind := range flush.Kinds() {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			spec, err := flush.Lookup(kind)
			require.NoError(t, err)

			assert.NotEmpty(t, spec.Command(""), "command")

			switch spec.Mode {
			case flush.ModeSession:
				assert.NotEmpty(t, spec.Identify, "identify")
				assert.NotNil(t, spec.Choose, "choose")
				assert.NotEmpty(t, spec.Probe, "probe")
				assert.NotEmpty(t, spec.Marker, "marker")
			case flush.ModeOneShot:
				assert.NotEmpty(t, spec.Note, "note")
			case flush.ModePair:
				assert.NotEmpty(t, spec.Note, "note")
				assert.NotEmpty(t, spec.ReleaseMarker, "release marker")
				// The release must be able to print its own marker, or the
				// check can never fire and a leaked lock passes as success.
				prefix, _, found := strings.Cut(spec.ReleaseMarker, "=")
				assert.True(t, found, "a release marker should pin a value, not just a word")
				assert.Contains(t, strings.Join(spec.Release(""), " "), prefix,
					"the release command must print its own marker")
				// The same for the lock: a refused lock can exit 0.
				prefix, _, found = strings.Cut(spec.LockMarker, "=")
				assert.True(t, found, "a lock marker should pin a value, not just a word")
				assert.Contains(t, strings.Join(spec.Command(""), " "), prefix,
					"the lock command must print its own marker")
			default:
				t.Fatalf("unknown mode %d", spec.Mode)
			}
		})
	}
}

// A marker that appears in the statement text can be matched by an error
// message that quotes the statement back, which a scan would read as the
// answer. Both are built with CONCAT so the joined marker exists only in what
// the server prints.
func TestMarkersAppearOnlyInTheServersAnswer(t *testing.T) {
	t.Parallel()

	spec, err := flush.Lookup("mysql")
	require.NoError(t, err)

	assert.NotContains(t, spec.Probe, spec.Marker)
	assert.NotContains(t, spec.Identify, flush.IdentityEnd)
	assert.NotContains(t, spec.Identify, "pv-migrate-server=")
}

// The two names reach one recipe: the server says what it is, and the lock is
// chosen from that, so --flush mysql against MariaDB works, and the reverse.
func TestMySQLAndMariaDBShareOneRecipe(t *testing.T) {
	t.Parallel()

	mysql, err := flush.Lookup("mysql")
	require.NoError(t, err)

	mariadb, err := flush.Lookup("mariadb")
	require.NoError(t, err)

	assert.Equal(t, mysql.Command("u"), mariadb.Command("u"))
	assert.Equal(t, mysql.Identify, mariadb.Identify)
}

// A session kind holds the client's stdin open across the snapshot, and both
// the MySQL and MariaDB clients block-buffer stdout into a pipe. Without
// --unbuffered the probe's marker stays in the client's buffer until the
// session ends, so the lock is taken but never confirmed. A reconnect drops
// every one of these locks, so it has to be an error instead.
func TestSessionClientsAreUnbufferedAndNeverReconnect(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"mysql", "mariadb"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			spec, err := flush.Lookup(kind)
			require.NoError(t, err)

			command := strings.Join(spec.Command(""), " ")
			assert.Contains(t, command, "--unbuffered")
			assert.Contains(t, command, "--skip-reconnect")
		})
	}
}

// Every identity below is what a real server returned for the Identify
// query, copied from the probe run against each image, so a change to the
// rules is checked against what servers actually say rather than against what
// they might be expected to.
func TestChooseMySQLLock(t *testing.T) {
	t.Parallel()

	const (
		lockInstance = "LOCK INSTANCE FOR BACKUP"
		lockTables   = "LOCK TABLES FOR BACKUP"
		ftwrl        = "FLUSH TABLES WITH READ LOCK"
		backupStage  = "BACKUP STAGE BLOCK_COMMIT"
	)

	for identity, want := range map[string]string{
		// Oracle's MySQL 5.7 has neither of the lighter locks.
		"5.7.44|MySQL Community Server (GPL)": ftwrl,
		// Percona 5.7 has its own backup lock, which Oracle's does not.
		"5.7.44-48|Percona Server (GPL), Release 48, Revision 497f936a373": lockTables,
		"8.0.46|MySQL Community Server - GPL":                              lockInstance,
		"8.0.46-37|Percona Server (GPL), Release 37, Revision 39e2b60e":    lockInstance,
		"8.4.11|MySQL Community Server - GPL":                              lockInstance,
		"8.4.11-11|Percona Server (GPL), Release 11, Revision 57878ff8":    lockInstance,
		"9.0.1|MySQL Community Server - GPL":                               lockInstance,
		"9.5.0|MySQL Community Server - GPL":                               lockInstance,
		// BACKUP STAGE arrived in MariaDB 10.4.
		"10.3.39-MariaDB-1:10.3.39+maria~ubu2004|mariadb.org binary distribution": ftwrl,
		"10.4.34-MariaDB-1:10.4.34+maria~ubu2004|mariadb.org binary distribution": backupStage,
		"10.5.29-MariaDB-ubu2004|mariadb.org binary distribution":                 backupStage,
		"10.6.28-MariaDB-ubu2204|mariadb.org binary distribution":                 backupStage,
		// 10.11 sorts before 10.4 as text; the rule has to compare numbers.
		"10.11.19-MariaDB-ubu2204|mariadb.org binary distribution": backupStage,
		"11.4.13-MariaDB-ubu2404|mariadb.org binary distribution":  backupStage,
		"11.8.9-MariaDB-ubu2404|mariadb.org binary distribution":   backupStage,
		"12.3.3-MariaDB-ubu2404|mariadb.org binary distribution":   backupStage,
		// A proxy in front of MariaDB can prepend the 5.5.5- that old
		// clients were given, which must not read as MySQL 5.5.
		"5.5.5-10.3.39-MariaDB|mariadb.org binary distribution": ftwrl,
		"5.5.5-10.6.28-MariaDB|mariadb.org binary distribution": backupStage,
		// Older than the verified range: the lock every version has.
		"5.6.51|MySQL Community Server (GPL)":          ftwrl,
		"5.6.51-91.0|Percona Server (GPL), Release 91": ftwrl,
	} {
		t.Run(identity, func(t *testing.T) {
			t.Parallel()

			lock, err := flush.ChooseMySQLLock(identity)
			require.NoError(t, err)
			assert.Equal(t, want, lock.Name)
			assert.NotEmpty(t, lock.Statement)
			assert.NotEmpty(t, lock.Unlock)
			assert.NotEmpty(t, lock.Note)
		})
	}
}

// An answer that names no version must not fall through to a default lock:
// guessing wrong in batch mode ends the session on the error.
func TestChooseMySQLLock_RefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	for _, identity := range []string{"", "|", "unknown|MySQL", "x.y|MySQL", "8|MySQL", "8.|MySQL"} {
		t.Run(identity, func(t *testing.T) {
			t.Parallel()

			_, err := flush.ChooseMySQLLock(identity)
			require.Error(t, err)
		})
	}
}

// Every lock is released by its own statement: LOCK INSTANCE by UNLOCK
// INSTANCE, the table locks by UNLOCK TABLES, the backup stages by BACKUP
// STAGE END. A mismatched unlock returns success and leaves the lock held.
func TestEachLockHasItsOwnUnlock(t *testing.T) {
	t.Parallel()

	for identity, unlock := range map[string]string{
		"8.4.11|MySQL Community Server - GPL":                    "UNLOCK INSTANCE;",
		"5.7.44-48|Percona Server (GPL), Release 48":             "UNLOCK TABLES;",
		"5.7.44|MySQL Community Server (GPL)":                    "UNLOCK TABLES;",
		"11.8.9-MariaDB-ubu2404|mariadb.org binary distribution": "BACKUP STAGE END;",
	} {
		lock, err := flush.ChooseMySQLLock(identity)
		require.NoError(t, err)
		assert.Equal(t, unlock, lock.Unlock, identity)
	}
}

func TestParseIdentity(t *testing.T) {
	t.Parallel()

	identity, err := flush.ParseIdentity(
		"some banner\npv-migrate-server=5.7.44-48|Percona Server (GPL)|pv-migrate-end\n")
	require.NoError(t, err)
	assert.Equal(t, "5.7.44-48|Percona Server (GPL)", identity)

	_, err = flush.ParseIdentity("ERROR 1045 (28000): Access denied for user 'backup'@'localhost'")
	require.Error(t, err, "no answer at all")
	assert.Contains(t, err.Error(), "Access denied", "the client's own words say why")

	_, err = flush.ParseIdentity("pv-migrate-server=8.4.11|MySQL Commu")
	require.Error(t, err, "an answer cut off before its end marker")
}

func TestCredentialsCheck(t *testing.T) {
	t.Parallel()

	//nolint:gosec // a made-up password, to show punctuation passes
	require.NoError(t, flush.Credentials{User: "backup", Password: "p@ss word;'\"$x"}.Check(),
		"anything on one line is carried as it is")
	require.NoError(t, flush.Credentials{}.Check())

	require.ErrorIs(t, flush.Credentials{Password: "a\nb"}.Check(), flush.ErrMultilinePassword)
	require.ErrorIs(t, flush.Credentials{Password: "a\r"}.Check(), flush.ErrMultilinePassword)
	require.Error(t, flush.Credentials{User: "a\nb"}.Check())
}

// A replaced command is not one of the scripts, so it reads no password line
// and none must be sent to it, or its first statement would be the password.
func TestWithCommandReadsNoCredentials(t *testing.T) {
	t.Parallel()

	spec, err := flush.Lookup("mysql")
	require.NoError(t, err)
	require.True(t, spec.ReadsCredentials)

	replaced := spec.WithCommand([]string{"mysql", "-uroot"})
	assert.False(t, replaced.ReadsCredentials)
	assert.Equal(t, []string{"mysql", "-uroot"}, replaced.Command("ignored"))
}
