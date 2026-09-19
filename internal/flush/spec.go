package flush

import (
	"fmt"
	"sort"
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
// Command runs in the database container, so it can rely on what that image
// provides and on the credentials in its environment. In ModeSession it starts
// a client that reads statements from stdin, and Lock, Probe and Unlock are
// sent to it; Marker is what Probe makes the client print, waited for before
// the cut because a statement written to stdin has only been buffered. In the
// other modes Command is the whole quiesce and Release is what runs after the
// cut, with ReleaseMarker proving it took.
type Spec struct {
	Mode    Mode
	Command []string

	Lock   string
	Probe  string
	Marker string
	Unlock string

	Release []string
	// ReleaseMarker is what Release must print for the release to count as
	// complete. It exists because a database that counts its locks can hold
	// one leaked by an earlier run, and a release that only reported "ok"
	// would leave that leak in place while claiming success.
	ReleaseMarker string

	// Note is printed when the lock is taken, for the one fact about this
	// database's quiesce an operator wants to know at that moment.
	Note string
}

// marker is what every session Probe prints. A fixed string keeps the scan
// simple and cannot collide with anything a database prints on its own.
const marker = "pv-migrate-quiesced"

// mongoClient wraps a mongosh --eval so that the root credentials the official
// image seeds are used when present and nothing is passed when they are not,
// since an empty -u breaks the connection. mongosh has no environment variable
// for the password, so it goes on the client's command line inside the pod.
func mongoClient(eval string) []string {
	return []string{
		"sh", "-c",
		`if [ -n "$MONGO_INITDB_ROOT_USERNAME" ]; then ` +
			`set -- -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin; ` +
			`fi; exec mongosh --quiet "$@" --eval '` + eval + `'`,
	}
}

// specs is the registry of supported databases, keyed by the --flush value.
var specs = map[string]Spec{
	"mysql": {
		Mode: ModeSession,
		// The password reaches the client through MYSQL_PWD, which the
		// official image does not set but does derive from
		// MYSQL_ROOT_PASSWORD, so both are honored without putting the
		// password on a command line where ps would show it.
		Command: []string{
			"sh", "-c",
			`export MYSQL_PWD="${MYSQL_PWD:-$MYSQL_ROOT_PASSWORD}"; ` +
				`exec mysql -uroot --batch --skip-column-names --unbuffered`,
		},
		// --unbuffered is required, not cosmetic: the client block-buffers its
		// stdout into a pipe, and this session holds stdin open across the
		// snapshot, so without it the probe's marker never leaves the client's
		// buffer and the lock can never be confirmed.
		//
		// LOCK INSTANCE FOR BACKUP blocks DDL while allowing DML, which is the
		// one thing redo recovery cannot repair on its own. Needs BACKUP_ADMIN.
		Lock:   "LOCK INSTANCE FOR BACKUP;",
		Probe:  "SELECT '" + marker + "';",
		Marker: marker,
		Unlock: "UNLOCK INSTANCE;",
		Note:   "DDL is blocked, writes continue; InnoDB recovers the rest from its redo log",
	},
	"mariadb": {
		Mode: ModeSession,
		// The 11+ image ships the mariadb client only; the mysql symlinks live
		// in a package it does not install. --skip-reconnect matters: the
		// client reconnects by default, and a reconnect silently drops the
		// backup stages, so a lost connection has to be a hard error instead.
		Command: []string{
			"sh", "-c",
			`export MYSQL_PWD="${MYSQL_PWD:-$MARIADB_ROOT_PASSWORD}"; ` +
				`exec mariadb --skip-reconnect -uroot --batch --skip-column-names --unbuffered`,
		},
		// --unbuffered for the same reason as mysql, see there.
		//
		// BLOCK_COMMIT is the documented stopping point: the skipped FLUSH
		// and BLOCK_DDL stages run implicitly, and when it returns nothing
		// further can commit, so the cut equals the last commit. Needs RELOAD.
		Lock:   "BACKUP STAGE START; BACKUP STAGE BLOCK_COMMIT;",
		Probe:  "SELECT '" + marker + "';",
		Marker: marker,
		Unlock: "BACKUP STAGE END;",
		Note: "non-transactional tables flushed, DDL blocked and commits held; " +
			"InnoDB and Aria recover the rest from their logs",
	},
	"postgres": {
		Mode: ModeOneShot,
		// A single-volume snapshot is crash-consistent through the WAL, so
		// nothing has to be held. CHECKPOINT only shortens recovery.
		// pg_backup_start is deliberately not used: its label file makes
		// recovery demand a record written after the cut, which the snapshot
		// cannot contain. Local connections in the official image are trust.
		Command: []string{
			"sh", "-c",
			`exec psql -X -q -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-postgres}" -c CHECKPOINT`,
		},
		Note: "checkpoint written to shorten recovery; the snapshot is crash-consistent through the WAL either way",
	},
	"mongodb": {
		Mode: ModePair,
		// fsyncLock flushes, checkpoints and blocks writes. The lock lives on
		// the server with a count, not in the connection, so two one-shot
		// calls are correct. The count is also why the release is verified:
		// a lock leaked by an earlier run leaves it at two, and one unlock
		// then leaves the database locked while reporting success.
		Command: mongoClient("db.fsyncLock()"),
		// The unlock reports the lock count it leaves behind, and zero is the
		// only value meaning the database is writable again.
		// db.currentOp().fsyncLock cannot be used for this: it reads
		// undefined even on a locked server, verified against mongo 8.0, so a
		// check built on it can never fire.
		Release:       mongoClient(`print("pv-migrate-lockcount=" + db.fsyncUnlock().lockCount)`),
		ReleaseMarker: "pv-migrate-lockcount=0",
		Note: "writes blocked and a checkpoint written; " +
			"on a replica set point this at a hidden secondary, not the primary",
	},
	"scylladb": {
		Mode: ModeOneShot,
		// flush is synchronous and holds nothing: memtables land in sealed
		// SSTables, and the commitlog on the same volume replays whatever
		// arrives between the flush and the cut.
		Command: []string{"nodetool", "flush"},
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
