package flush

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Mode says how a kind's quiesce is driven, which follows from whether the
// database keeps the lock in the session that took it, on the server, or not
// at all.
type Mode int

const (
	// ModeSession holds one client process open on its stdin across the cut.
	// The lock dies with that session, so the session has to outlive the cut.
	ModeSession Mode = iota
	// ModeOneShot runs one command before the cut and holds nothing. The
	// database keeps no state to release.
	ModeOneShot
	// ModePair runs one command before the cut and another after it. The
	// database holds the lock on the server, so no connection stays open,
	// but the release has to run on every path or the database stays locked.
	ModePair
)

// Spec says how to quiesce one kind of database from its own pod.
//
// Every command runs in the database container, so it can rely on what that
// image provides. Which client binary an image ships differs between
// versions of the same database, so each command looks for the ones it can
// use rather than naming one.
//
// In ModeSession the client reads statements from stdin: Identify first,
// whose answer Choose turns into the lock for that server, then the lock and
// Probe. Marker is what Probe makes the client print, waited for before the
// cut because a statement written to stdin has only been buffered. In the
// other modes the command is the whole quiesce, and in ModePair the release
// runs after the cut with ReleaseMarker proving it took.
type Spec struct {
	Mode Mode

	// ReadsCredentials reports that the command reads a password as the
	// first line of its stdin. False for a kind that never logs in, and for
	// a command the caller replaced, which knows nothing of that line.
	ReadsCredentials bool

	Identify string
	Choose   func(identity string) (Lock, error)
	Probe    string
	Marker   string

	// LockMarker is what a ModePair lock command must print for the lock to
	// count as taken. A command that exits 0 is not enough: mongo 4.4's
	// legacy shell returns ok: 0 from a refused fsyncLock and exits 0, so an
	// unchecked lock reports success while nothing is locked and the snapshot
	// runs unprotected.
	LockMarker string

	// ReleaseMarker is what the release must print for it to count as
	// complete. It exists because a database that counts its locks can hold
	// one leaked by an earlier run, and a release that only reported "ok"
	// would leave that leak in place while claiming success.
	ReleaseMarker string

	// Note is printed when the lock is taken, for the one fact about this
	// database's quiesce an operator wants to know at that moment. A session
	// kind takes its note from the Lock chosen for the server instead.
	Note string

	command  func(user string) []string
	release  func(user string) []string
	override []string
}

// Command returns the client command, logging in as user. An empty user
// falls back to what the database's own image seeds.
func (s Spec) Command(user string) []string {
	if s.override != nil {
		return s.override
	}

	return s.command(user)
}

// Release returns the command that undoes a ModePair lock.
func (s Spec) Release(user string) []string {
	return s.release(user)
}

// WithCommand replaces the client command. The replacement reads no password
// line, since it is not one of the scripts below, so none is sent to it.
func (s Spec) WithCommand(argv []string) Spec {
	s.override = argv
	s.ReadsCredentials = false

	return s
}

// Lock is the statement that quiesces one particular server, chosen from what
// it reports about itself, and the one that releases it.
type Lock struct {
	Name      string
	Statement string
	Unlock    string
	Note      string
}

// Credentials is who a built-in client logs in as.
//
// The password is never put on a command line. Every script below reads it
// as the first line of its stdin and hands it to the client the way the
// client prefers, which for mysql and psql is an environment variable. On a
// command line it would sit in the exec request, and so in the API server's
// audit log, and in the pod's process table.
type Credentials struct {
	User     string
	Password string
}

// ErrMultilinePassword refuses a password the stdin protocol cannot carry.
// The password is the first line; anything after a newline would reach the
// database client as a statement, and run as one.
var ErrMultilinePassword = errors.New("the flush password contains a line break, which cannot be passed safely")

// Check reports whether the credentials can be sent.
func (c Credentials) Check() error {
	if strings.ContainsAny(c.Password, "\r\n") {
		return ErrMultilinePassword
	}

	if strings.ContainsAny(c.User, "\r\n") {
		return errors.New("the flush user contains a line break")
	}

	return nil
}

// The identity query and the probe build their markers with CONCAT, so the
// marker only exists in what the server prints and never in the statement
// text. A client that echoed a failing statement back in its error message
// would otherwise show the marker too, and a scan would take that for an
// answer.
const (
	identityPrefix = "pv-migrate-server="
	// IdentityEnd closes the line Identify prints, and is what a session
	// waits for before reading the answer.
	IdentityEnd = "|pv-migrate-end"
	marker      = "pv-migrate-quiesced"
)

const mysqlIdentify = "SELECT CONCAT('pv-migrate', '-server=', @@version, '|', " +
	"@@version_comment, '|pv-migrate', '-end');"

const mysqlProbe = "SELECT CONCAT('pv-migrate', '-quiesced');"

// ParseIdentity pulls the server's answer to Identify out of the output.
func ParseIdentity(output string) (string, error) {
	_, rest, found := strings.Cut(output, identityPrefix)
	if !found {
		return "", fmt.Errorf("the server did not say what it is; output: %s", output)
	}

	answer, _, found := strings.Cut(rest, IdentityEnd)
	if !found {
		return "", fmt.Errorf("the server's answer was cut short; output: %s", output)
	}

	return answer, nil
}

// mysqlScript starts whichever of the mariadb and mysql clients the image has
// and logs in. The two are the same program under two names: MySQL images
// and MariaDB 10.3 ship only mysql, and MariaDB 11.4 onwards only mariadb.
//
// The password falls back to what the images seed, MARIADB_ROOT_PASSWORD
// before MYSQL_ROOT_PASSWORD because MariaDB images accept both.
//
// --unbuffered is required, not cosmetic: the client block-buffers its stdout
// into a pipe, and the session holds stdin open across the snapshot, so
// without it the probe's marker never leaves the client's buffer and the lock
// can never be confirmed. --skip-reconnect because a reconnect silently drops
// every one of these locks, so a lost connection has to be a hard error.
const mysqlScript = `IFS= read -r pw; ` +
	`export MYSQL_PWD="${pw:-${MYSQL_PWD:-${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}}}"; ` +
	`client=$(command -v mariadb || command -v mysql) || ` +
	`{ echo "pv-migrate: this container has neither a mariadb nor a mysql client" >&2; exit 127; }; ` +
	`exec "$client" --skip-reconnect -u"${1:-root}" --batch --skip-column-names --unbuffered`

// postgresScript runs CHECKPOINT. The database is named because a user given
// with --flush-user often has no database of its own name, and postgres is
// the one initdb always creates. With no password given, the one the image
// seeded is used, as for MySQL and MongoDB. PGPASSWORD is set only when there
// is one: the official image trusts local connections by default, and an
// empty PGPASSWORD would still be tried. -w because a server that wants a
// password psql was not given would otherwise make it prompt for one on a
// terminal it does not have, rather than fail with the reason.
const postgresScript = `IFS= read -r pw; ` +
	`pw="${pw:-${PGPASSWORD:-$POSTGRES_PASSWORD}}"; ` +
	`if [ -n "$pw" ]; then export PGPASSWORD="$pw"; fi; ` +
	`exec psql -X -q -w -v ON_ERROR_STOP=1 -d postgres -U "${1:-${POSTGRES_USER:-postgres}}" -c CHECKPOINT`

// mongoScript evaluates the JavaScript in $2 with whichever shell the image
// has: mongo 4.4 ships only the legacy mongo, 6.0 onwards only mongosh, and
// both take the same flags for this. mongosh reads a password from nowhere
// but its command line, so inside the pod that is where it goes. Without a
// user it connects unauthenticated, since an empty -u breaks the connection.
const mongoScript = `IFS= read -r pw; ` +
	`user="${1:-$MONGO_INITDB_ROOT_USERNAME}"; pass="${pw:-$MONGO_INITDB_ROOT_PASSWORD}"; js="$2"; ` +
	`client=$(command -v mongosh || command -v mongo) || ` +
	`{ echo "pv-migrate: this container has neither mongosh nor mongo" >&2; exit 127; }; ` +
	`if [ -n "$user" ]; then ` +
	`exec "$client" --quiet -u "$user" -p "$pass" --authenticationDatabase admin --eval "$js"; fi; ` +
	`exec "$client" --quiet --eval "$js"`

func shellScript(script string, args ...string) []string {
	return append([]string{"sh", "-c", script, "sh"}, args...)
}

// The locks the MySQL family offers, and what each costs.
var (
	lockInstance = Lock{
		Name:      "LOCK INSTANCE FOR BACKUP",
		Statement: "LOCK INSTANCE FOR BACKUP;",
		Unlock:    "UNLOCK INSTANCE;",
		Note:      "DDL is blocked, writes continue; InnoDB recovers the rest from its redo log",
	}
	lockTablesForBackup = Lock{
		Name:      "LOCK TABLES FOR BACKUP",
		Statement: "LOCK TABLES FOR BACKUP;",
		Unlock:    "UNLOCK TABLES;",
		Note: "DDL and writes to non-transactional tables are blocked, InnoDB writes continue; " +
			"InnoDB recovers the rest from its redo log",
	}
	flushWithReadLock = Lock{
		Name:      "FLUSH TABLES WITH READ LOCK",
		Statement: "FLUSH TABLES WITH READ LOCK;",
		Unlock:    "UNLOCK TABLES;",
		Note: "all writes are blocked until the snapshot is cut; the lock waits for running " +
			"statements to finish first, so a long query delays it and stalls writers meanwhile",
	}
	backupStage = Lock{
		Name:      "BACKUP STAGE BLOCK_COMMIT",
		Statement: "BACKUP STAGE START; BACKUP STAGE BLOCK_COMMIT;",
		Unlock:    "BACKUP STAGE END;",
		Note: "non-transactional tables flushed, DDL blocked and commits held; " +
			"InnoDB and Aria recover the rest from their logs",
	}
)

// ChooseMySQLLock picks the lock for a MySQL-family server from its version
// and version comment, as Identify reports them.
//
// Each rule is the oldest server the statement works on, verified against
// the images: LOCK INSTANCE FOR BACKUP from MySQL and Percona 8.0; Percona's
// LOCK TABLES FOR BACKUP on 5.7, where Oracle's MySQL has only FLUSH TABLES
// WITH READ LOCK; BACKUP STAGE from MariaDB 10.4, with 10.3 back on FLUSH
// TABLES WITH READ LOCK. The server is asked rather than the statements tried
// in turn, because in batch mode the client exits on the first error and
// takes the session, and any lock it held, with it.
func ChooseMySQLLock(identity string) (Lock, error) {
	version, comment, _ := strings.Cut(identity, "|")

	major, minor, err := leadingVersion(version)
	if err != nil {
		return Lock{}, fmt.Errorf("cannot tell which lock %q supports: %w", identity, err)
	}

	switch {
	case strings.Contains(version, "MariaDB"):
		if atLeast(major, minor, 10, 4) {
			return backupStage, nil
		}

		return flushWithReadLock, nil
	case major >= 8:
		return lockInstance, nil
	case strings.Contains(comment, "Percona") && major == 5 && minor >= 7:
		return lockTablesForBackup, nil
	default:
		return flushWithReadLock, nil
	}
}

func atLeast(major, minor, wantMajor, wantMinor int) bool {
	return major > wantMajor || (major == wantMajor && minor >= wantMinor)
}

// leadingVersion reads the major and minor number a server version starts
// with. A proxy in front of MariaDB can report "5.5.5-10.3.39-MariaDB", the
// prefix old clients were once given, which is skipped to reach the real one.
func leadingVersion(version string) (int, int, error) {
	version = strings.TrimSpace(version)

	if rest, ok := strings.CutPrefix(version, "5.5.5-"); ok && strings.Contains(rest, "MariaDB") {
		version = rest
	}

	majorPart, rest, ok := strings.Cut(version, ".")
	if !ok {
		return 0, 0, fmt.Errorf("no major.minor in %q", version)
	}

	minorPart := rest
	if i := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		minorPart = rest[:i]
	}

	major, err := strconv.Atoi(majorPart)
	if err != nil {
		return 0, 0, fmt.Errorf("no major version in %q", version)
	}

	minor, err := strconv.Atoi(minorPart)
	if err != nil {
		return 0, 0, fmt.Errorf("no minor version in %q", version)
	}

	return major, minor, nil
}

// mysqlFamily is the spec for both --flush mysql and --flush mariadb. The two
// names are kept for the user's benefit; the server says what it is, and the
// lock is chosen from that, so either name works against either server.
func mysqlFamily() Spec {
	return Spec{
		Mode:             ModeSession,
		ReadsCredentials: true,
		command:          func(user string) []string { return shellScript(mysqlScript, user) },
		Identify:         mysqlIdentify,
		Choose:           ChooseMySQLLock,
		Probe:            mysqlProbe,
		Marker:           marker,
	}
}

// specs is the registry of supported databases, keyed by the --flush value.
var specs = map[string]Spec{
	"mysql":   mysqlFamily(),
	"mariadb": mysqlFamily(),
	"postgres": {
		Mode:             ModeOneShot,
		ReadsCredentials: true,
		// A single-volume snapshot is crash-consistent through the WAL, so
		// nothing has to be held; CHECKPOINT only shortens recovery. The same
		// on every version from 11 to 18. pg_backup_start is deliberately not
		// used: its label file makes recovery demand a record written after
		// the cut, which the snapshot cannot contain.
		command: func(user string) []string { return shellScript(postgresScript, user) },
		Note: "checkpoint written to shorten recovery; " +
			"the snapshot is crash-consistent through the WAL either way",
	},
	"mongodb": {
		Mode:             ModePair,
		ReadsCredentials: true,
		// fsyncLock flushes, checkpoints and blocks writes. The lock lives on
		// the server with a count, not in the connection, so two one-shot
		// calls are correct. The count is also why the release is verified:
		// a lock leaked by an earlier run leaves it at two, and one unlock
		// then leaves the database locked while reporting success.
		command: func(user string) []string {
			return shellScript(mongoScript, user,
				`var r = db.fsyncLock(); print("pv-migrate-fsynclock=" + r.ok + " " + (r.errmsg || ""))`)
		},
		LockMarker: "pv-migrate-fsynclock=1",
		// The unlock reports the lock count it leaves behind, and zero is the
		// only value meaning the database is writable again.
		// db.currentOp().fsyncLock cannot be used for this: it reads
		// undefined even on a locked server, verified against mongo 8.0, so a
		// check built on it can never fire.
		release: func(user string) []string {
			return shellScript(mongoScript, user, `print("pv-migrate-lockcount=" + db.fsyncUnlock().lockCount)`)
		},
		ReleaseMarker: "pv-migrate-lockcount=0",
		Note: "writes blocked and a checkpoint written; " +
			"on a replica set point this at a hidden secondary, not the primary",
	},
	"scylladb": {
		Mode: ModeOneShot,
		// nodetool reaches the node through its local REST API, not CQL, so
		// it works with CQL authentication switched on and logs in as nobody.
		// flush is synchronous and holds nothing: memtables land in sealed
		// SSTables, and the commitlog on the same volume replays whatever
		// arrives between the flush and the cut.
		command: func(string) []string { return []string{"nodetool", "flush"} },
		Note: "memtables flushed to SSTables; the commitlog replays anything after, " +
			"up to 10 s of acknowledged writes under the default periodic sync",
	},
}

// Lookup returns the Spec for a --flush value.
func Lookup(kind string) (Spec, error) {
	spec, ok := specs[kind]
	if !ok {
		return Spec{}, fmt.Errorf("unsupported --flush %q, must be one of %s",
			kind, strings.Join(Kinds(), ", "))
	}

	return spec, nil
}

// Kinds returns the supported --flush values, sorted, for help text and errors.
func Kinds() []string {
	kinds := make([]string, 0, len(specs))
	for kind := range specs {
		kinds = append(kinds, kind)
	}

	sort.Strings(kinds)

	return kinds
}
