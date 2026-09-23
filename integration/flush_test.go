//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/utkuozdemir/pv-migrate/internal/flush"
	"github.com/utkuozdemir/pv-migrate/internal/k8s"
	"github.com/utkuozdemir/pv-migrate/pvmigrate"
)

// databaseReadyTimeout is generous because these images are large and the
// first boot initialises a data directory.
const databaseReadyTimeout = 5 * time.Minute

// database describes one server the flush tests stand up, and how to talk to
// it well enough to seed, count and check that it is writable.
type database struct {
	kind     string
	image    string
	dataPath string
	// subPath mounts a directory of the claim rather than its root, for a
	// server that refuses to initialise next to the filesystem's lost+found.
	subPath string
	// fsGroup makes the claim writable by an image that runs as a user of
	// its own.
	fsGroup *int64
	// args are the server's own start-up arguments, which only ScyllaDB needs
	// in order to fit on a test node.
	args      []string
	env       []corev1.EnvVar
	probeCmd  string
	seedCmd   string
	countCmd  string
	insertCmd string
}

// The fixtures' own commands find their client the way the flush scripts do,
// because the binary differs between versions of one database: MariaDB 10.3
// ships only mysql and 11.4 onwards only mariadb, mongo 4.4 only the legacy
// mongo shell and 6.0 onwards only mongosh.
const (
	sqlClient   = `"$(command -v mariadb || command -v mysql)"`
	mongoClient = `"$(command -v mongosh || command -v mongo)"`
	// The images seed a root password for either database family through
	// MYSQL_ROOT_PASSWORD, which MariaDB images accept alongside their own.
	sqlRoot = `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" ` + sqlClient + ` -uroot --batch --skip-column-names`
	// PGPASSWORD is always set, so the same commands work whether local
	// connections are trusted, as the image does by default, or not, as a
	// credential case sets up.
	pgRoot    = `PGPASSWORD="$POSTGRES_PASSWORD" psql -U postgres`
	mongoRoot = mongoClient + ` -u root -p pvmtest --authenticationDatabase admin`
)

// mysqlFamilyDatabase is a MySQL, Percona or MariaDB server of any version.
// kind is the --flush name the case runs with; the lock is chosen from what
// the server reports, so the name does not decide it.
func mysqlFamilyDatabase(kind, image string) database {
	var fsGroup *int64
	if strings.HasPrefix(image, "percona/") {
		// Percona's images run as mysql, uid and gid 1001, where the others
		// start as root.
		fsGroup = new(int64(1001))
	}

	return database{
		kind:     kind,
		image:    image,
		dataPath: "/var/lib/mysql",
		// MySQL 5.7's --initialize aborts on a data directory with anything
		// in it, lost+found included.
		subPath: "mysql",
		fsGroup: fsGroup,
		env: []corev1.EnvVar{
			{Name: "MYSQL_ROOT_PASSWORD", Value: "pvmtest"},
			{Name: "MYSQL_DATABASE", Value: "app"},
		},
		// Over TCP, because the entrypoint first starts a server that listens
		// on the socket alone to initialise the data directory, then restarts
		// it; a socket probe can answer from the first one and the seed then
		// meets the restart.
		// Its stderr is dropped because the MariaDB 11.4 and later clients
		// warn over TCP that they skip verifying the server certificate,
		// and the exec helper takes any stderr as a failure.
		probeCmd: sqlRoot + ` -h127.0.0.1 -P3306 -e 'SELECT 1' 2>/dev/null`,
		seedCmd: sqlRoot + ` app -e ` +
			`'CREATE TABLE IF NOT EXISTS t(id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(64)); ` +
			`INSERT INTO t(v) VALUES ("seeded")'`,
		countCmd:  sqlRoot + ` app -e 'SELECT COUNT(*) FROM t'`,
		insertCmd: sqlRoot + ` app -e 'INSERT INTO t(v) VALUES ("after-backup")'`,
	}
}

func postgresDatabase(image string) database {
	psql := pgRoot

	return database{
		kind:     "postgres",
		image:    image,
		dataPath: "/var/lib/postgresql/data",
		env: []corev1.EnvVar{
			{Name: "POSTGRES_PASSWORD", Value: "pvmtest"},
			{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
		},
		// Over TCP, for the same reason as MySQL: initdb runs a server that
		// listens on no address before the real one starts.
		probeCmd: psql + ` -h 127.0.0.1 -tAc 'SELECT 1'`,
		seedCmd: psql + ` -q ` +
			`-c 'CREATE TABLE IF NOT EXISTS t(id serial primary key, v text)' ` +
			`-c "INSERT INTO t(v) VALUES ('seeded')"`,
		countCmd:  psql + ` -tAc 'SELECT COUNT(*) FROM t'`,
		insertCmd: psql + ` -q -c "INSERT INTO t(v) VALUES ('after-backup')"`,
	}
}

// mongoDatabase runs without authentication, the image's default when no
// root user is seeded.
func mongoDatabase(image string) database {
	return mongoWith(image, mongoClient)
}

// mongoAuthDatabase seeds a root user, which makes the image start the server
// with authentication on.
func mongoAuthDatabase(image string) database {
	db := mongoWith(image, mongoRoot)
	db.env = []corev1.EnvVar{
		{Name: "MONGO_INITDB_ROOT_USERNAME", Value: "root"},
		{Name: "MONGO_INITDB_ROOT_PASSWORD", Value: "pvmtest"},
	}

	return db
}

func mongoWith(image, client string) database {
	return database{
		kind:      "mongodb",
		image:     image,
		dataPath:  "/data/db",
		probeCmd:  client + ` --quiet --eval 'db.adminCommand({ping: 1}).ok' | grep -qx 1`,
		seedCmd:   client + ` --quiet --eval 'db.t.insertOne({v:"seeded"})'`,
		countCmd:  client + ` --quiet --eval 'print(db.t.countDocuments({}))'`,
		insertCmd: client + ` --quiet --eval 'db.t.insertOne({v:"after-backup"})'`,
	}
}

// cqlsh keeps what cqlsh writes to stderr unless it fails. Its driver can
// print a traceback while the interpreter shuts down after a statement that
// succeeded, and the exec helper takes any stderr as a failure.
const cqlsh = `run() { "$@" 2>/tmp/cqlsh.err || { rc=$?; cat /tmp/cqlsh.err >&2; exit $rc; }; }; run cqlsh`

func scyllaDatabase(image string) database {
	return database{
		kind:     "scylladb",
		image:    image,
		dataPath: "/var/lib/scylla",
		// From 2025.1 the image runs as scylla, uid 999 in group 1000.
		fsGroup: new(int64(1000)),
		// The address is pinned because the image derives it from
		// `hostname -i` when it is not given, and interpolates the result
		// into --listen-address unquoted. On a dual-stack pod that command
		// prints both addresses, so Scylla is handed two positional
		// arguments and refuses to start. Nothing here reaches this node
		// over the network; the probe and the queries exec into its pod.
		args: []string{
			"--smp", "1", "--memory", "1G", "--overprovisioned", "1", "--developer-mode", "1",
			"--listen-address", "127.0.0.1", "--rpc-address", "127.0.0.1", "--seeds", "127.0.0.1",
		},
		// A node reports UN before it listens for CQL, which 5.4 takes a
		// while longer to do, and the seed goes through CQL.
		probeCmd: `nodetool status | grep -q '^UN' && ` + cqlsh + ` -e 'SELECT now() FROM system.local'`,
		seedCmd: cqlsh + ` -e "CREATE KEYSPACE IF NOT EXISTS app WITH replication=` +
			`{'class':'SimpleStrategy','replication_factor':1}; ` +
			`CREATE TABLE IF NOT EXISTS app.t(id int PRIMARY KEY, v text); ` +
			`INSERT INTO app.t(id,v) VALUES (1,'seeded');"`,
		countCmd:  cqlsh + ` -e "SELECT COUNT(*) FROM app.t" | sed -n '4p' | tr -d ' '`,
		insertCmd: cqlsh + ` -e "INSERT INTO app.t(id,v) VALUES (2,'after-backup')"`,
	}
}

// flushMatrix is every version of every database --flush supports, from the
// oldest still in use to the newest released. The lock differs across them,
// and so does the client binary, so each one is its own case.
func flushMatrix() []database {
	return []database{
		mysqlFamilyDatabase("mysql", "mysql:5.7"),
		mysqlFamilyDatabase("mysql", "percona/percona-server:5.7"),
		mysqlFamilyDatabase("mysql", "mysql:8.0"),
		mysqlFamilyDatabase("mysql", "percona/percona-server:8.0"),
		mysqlFamilyDatabase("mysql", "mysql:8.4"),
		mysqlFamilyDatabase("mysql", "percona/percona-server:8.4"),
		mysqlFamilyDatabase("mysql", "mysql:9.0"),
		mysqlFamilyDatabase("mysql", "mysql:9.5"),
		mysqlFamilyDatabase("mariadb", "mariadb:10.3"),
		mysqlFamilyDatabase("mariadb", "mariadb:10.4"),
		mysqlFamilyDatabase("mariadb", "mariadb:10.6"),
		mysqlFamilyDatabase("mariadb", "mariadb:10.11"),
		mysqlFamilyDatabase("mariadb", "mariadb:11.4"),
		mysqlFamilyDatabase("mariadb", "mariadb:11.8"),
		mysqlFamilyDatabase("mariadb", "mariadb:12"),
		postgresDatabase("postgres:11"),
		postgresDatabase("postgres:12"),
		postgresDatabase("postgres:13"),
		postgresDatabase("postgres:14"),
		postgresDatabase("postgres:15"),
		postgresDatabase("postgres:16"),
		postgresDatabase("postgres:17"),
		postgresDatabase("postgres:18"),
		mongoDatabase("mongo:4.4"),
		mongoDatabase("mongo:5.0"),
		mongoDatabase("mongo:6.0"),
		mongoDatabase("mongo:7.0"),
		mongoDatabase("mongo:8.0"),
		mongoDatabase("mongo:8.2"),
	}
}

// scyllaMatrix runs one at a time: every instance claims its share of the
// host-wide fs.aio-max-nr, and a few on one node exhaust it.
func scyllaMatrix() []database {
	return []database{
		scyllaDatabase("scylladb/scylla:5.4"),
		scyllaDatabase("scylladb/scylla:6.2"),
		scyllaDatabase("scylladb/scylla:2025.3"),
	}
}

func TestFlush(t *testing.T) {
	t.Parallel()

	infra := setupSnapshotInfra(t)

	for _, db := range flushMatrix() {
		t.Run(db.image, func(t *testing.T) {
			t.Parallel()
			testFlushRoundTrip(t, infra, db)
		})
	}

	t.Run("scylladb", func(t *testing.T) {
		t.Parallel()

		for _, db := range scyllaMatrix() {
			//nolint:paralleltest // one at a time, see scyllaMatrix
			t.Run(db.image, func(t *testing.T) {
				testFlushRoundTrip(t, infra, db)
			})
		}
	})

	for _, cred := range credentialCases() {
		t.Run("Credentials/"+cred.db.image, func(t *testing.T) {
			t.Parallel()
			testFlushCredentials(t, infra, cred)
		})
	}

	t.Run("MongoLeakedLockIsCaught", func(t *testing.T) {
		t.Parallel()
		testFlushMongoLeakedLock(t, infra)
	})

	t.Run("EveryKindIsCovered", func(t *testing.T) {
		t.Parallel()

		seen := map[string]bool{}
		for _, db := range append(flushMatrix(), scyllaMatrix()...) {
			seen[db.kind] = true
		}

		covered := make([]string, 0, len(seen))
		for kind := range seen {
			covered = append(covered, kind)
		}

		sort.Strings(covered)
		assert.Equal(t, flush.Kinds(), covered,
			"every kind --flush accepts needs a case here, or it ships with no test at all")
	})

	t.Run("UnknownKindIsRefused", func(t *testing.T) {
		t.Parallel()
		testFlushUnknownKind(t, infra)
	})

	t.Run("CommandOverrideRefusedForPairKinds", func(t *testing.T) {
		t.Parallel()
		testFlushCommandOverrideRefused(t, infra)
	})
}

// backupPassword carries a space and shell and SQL punctuation, so a
// password is shown to reach the database as one value from end to end.
const backupPassword = "b pw;#%"

// credentialCase is a database with authentication that the flush has to log
// in to as a user of its own, holding only the privilege its lock needs.
type credentialCase struct {
	db database
	// setupCmd creates user with backupPassword and the privilege the lock
	// needs, and unprivileged with password "bpw" and less than that.
	setupCmd     string
	user         string
	password     string
	unprivileged string
	// unprivilegedErr is what the refusal has to say, where only one part of
	// the code can catch it.
	unprivilegedErr string
}

// mysqlUsers creates a backup user holding grant, and a second one holding
// only lesser, or nothing when lesser is empty.
func mysqlUsers(grant, lesser string) string {
	sql := "CREATE USER 'backup'@'localhost' IDENTIFIED BY '" + backupPassword + "'; " +
		"GRANT " + grant + " ON *.* TO 'backup'@'localhost'; " +
		"CREATE USER 'nopriv'@'localhost' IDENTIFIED BY 'bpw';"
	if lesser != "" {
		sql += " GRANT " + lesser + " ON *.* TO 'nopriv'@'localhost';"
	}

	return sqlRoot + ` -e "` + sql + `"`
}

// postgresAuthDatabase makes local connections need a password, which the
// image otherwise trusts, so a flush without the right one cannot pass.
func postgresAuthDatabase(image, method string) database {
	db := postgresDatabase(image)
	db.env = append(db.env, corev1.EnvVar{Name: "POSTGRES_INITDB_ARGS", Value: "--auth-local=" + method})

	return db
}

func credentialCases() []credentialCase {
	mongoUsers := mongoRoot + ` --quiet --eval '` +
		`db.getSiblingDB("admin").createUser({user: "backup", pwd: "` + backupPassword + `", roles: ["hostManager"]}); ` +
		`db.getSiblingDB("admin").createUser({user: "nopriv", pwd: "bpw", roles: ["readAnyDatabase"]})'`

	return []credentialCase{
		{
			db: mysqlFamilyDatabase("mysql", "percona/percona-server:5.7"),
			// LOCK TABLES is the privilege the name suggests, and not the one
			// LOCK TABLES FOR BACKUP needs.
			setupCmd: mysqlUsers("RELOAD", "LOCK TABLES"),
			user:     "backup", password: backupPassword, unprivileged: "nopriv",
		},
		{
			db: mysqlFamilyDatabase("mysql", "mysql:8.4"),
			// RELOAD is enough for every older lock and not for this one.
			setupCmd: mysqlUsers("BACKUP_ADMIN", "RELOAD"),
			user:     "backup", password: backupPassword, unprivileged: "nopriv",
		},
		{
			db:       mysqlFamilyDatabase("mariadb", "mariadb:10.3"),
			setupCmd: mysqlUsers("RELOAD", ""),
			user:     "backup", password: backupPassword, unprivileged: "nopriv",
		},
		{
			db:       mysqlFamilyDatabase("mariadb", "mariadb:11.8"),
			setupCmd: mysqlUsers("RELOAD", ""),
			user:     "backup", password: backupPassword, unprivileged: "nopriv",
		},
		{
			db: postgresAuthDatabase("postgres:18", "scram-sha-256"),
			setupCmd: pgRoot + ` -q -c "CREATE ROLE backup LOGIN PASSWORD '` + backupPassword + `'" ` +
				`-c "GRANT pg_checkpoint TO backup" -c "CREATE ROLE nopriv LOGIN PASSWORD 'bpw'"`,
			user: "backup", password: backupPassword, unprivileged: "nopriv",
		},
		{
			// Before 15 there is no pg_checkpoint, so only a superuser can.
			db:       postgresAuthDatabase("postgres:11", "md5"),
			setupCmd: pgRoot + ` -q -c "CREATE ROLE nopriv LOGIN PASSWORD 'bpw'"`,
			user:     "postgres", password: "pvmtest", unprivileged: "nopriv",
		},
		{
			db:       mongoAuthDatabase("mongo:8.0"),
			setupCmd: mongoUsers,
			user:     "backup", password: backupPassword, unprivileged: "nopriv",
		},
		{
			// The legacy shell answers a refused fsyncLock with ok: 0 and
			// exits 0, so only the lock's own output can catch it.
			db:       mongoAuthDatabase("mongo:4.4"),
			setupCmd: mongoUsers,
			user:     "backup", password: backupPassword, unprivileged: "nopriv",
			unprivilegedErr: "refused the lock",
		},
	}
}

// testFlushCredentials logs the flush in as a user of its own. A wrong
// password and a user without the privilege must each fail the backup before
// a snapshot is cut, leave the database writable and leave nothing behind;
// the right user, with its password read from a Secret, must succeed.
//
//nolint:thelper // subtest implementation, not a helper
func testFlushCredentials(t *testing.T, infra *snapshotInfra, cred credentialCase) {
	db := cred.db
	place := newPlacement(t, infra.cli)
	ns := newTestNS(t, infra.cli, "pvmig-cred")

	require.NoError(t, provisionDatabase(t.Context(), infra.cli, ns, "db-pvc", "db", db, place))
	waitForDatabase(t, infra, ns, "db", db)

	_, err := execInPod(t.Context(), infra.cli, ns, "db", db.seedCmd)
	require.NoError(t, err)

	out, err := execInPod(t.Context(), infra.cli, ns, "db", cred.setupCmd)
	require.NoError(t, err, out)

	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")

	_, err = infra.cli.KubeClient.CoreV1().Secrets(ns).Create(t.Context(), &corev1.Secret{
		Name:       "db-flush",
		Namespace:  ns,
		StringData: map[string]string{"password": cred.password, "nopriv": "bpw"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	backupAs := func(file, user string) pvmigrate.Backup {
		backup := snapshotBackup(t, infra, ns, "archive-pvc:/"+file)
		backup.PVC.Name = "db-pvc"
		backup.Flush = db.kind
		backup.IgnoreMounted = false
		backup.NoCleanupOnFailure = false
		backup.FlushUser = user

		return backup
	}

	wrong := backupAs("wrong.tar.zst", cred.user)
	wrong.FlushPassword = "wrong " + cred.password
	assertFlushRefused(t, infra, ns, cred, "wrong password", wrong, "")

	unprivileged := backupAs("nopriv.tar.zst", cred.unprivileged)
	unprivileged.FlushPasswordSecret = "db-flush:nopriv"
	assertFlushRefused(t, infra, ns, cred, "unprivileged user", unprivileged, cred.unprivilegedErr)

	backup := backupAs("ok.tar.zst", cred.user)
	backup.FlushPasswordSecret = "db-flush"
	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup))

	_, err = execInPod(t.Context(), infra.cli, ns, "db", db.insertCmd)
	require.NoError(t, err, "the database must be writable once the quiesce is released")
}

// testFlushRoundTrip is the whole point of --flush: the database is quiesced
// only while the snapshot is cut, it is writable again straight after, and the
// archive taken from the clone restores to a volume that boots.
//
//nolint:thelper // subtest implementation, not a helper
func testFlushRoundTrip(t *testing.T, infra *snapshotInfra, db database) {
	place := newPlacement(t, infra.cli)
	ns := newTestNS(t, infra.cli, "pvmig-flush")

	require.NoError(t, provisionDatabase(t.Context(), infra.cli, ns, "db-pvc", "db", db, place))
	waitForDatabase(t, infra, ns, "db", db)

	_, err := execInPod(t.Context(), infra.cli, ns, "db", db.seedCmd)
	require.NoError(t, err)

	seeded := databaseCount(t, infra, ns, "db", db)
	require.NotEmpty(t, seeded)

	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")

	archiveFile := "archive-pvc:/" + db.kind + ".tar.zst"

	backup := snapshotBackup(t, infra, ns, archiveFile)
	backup.PVC.Name = "db-pvc"
	backup.Flush = db.kind
	// The live claim is mounted by the database, which is exactly the case a
	// snapshot-backed run is for.
	backup.IgnoreMounted = false

	require.NoError(t, pvmigrate.RunBackup(t.Context(), backup))

	// The release has to have run, or the database would still be blocked.
	_, err = execInPod(t.Context(), infra.cli, ns, "db", db.insertCmd)
	require.NoError(t, err, "the database must be writable once the quiesce is released")

	// Stop the original before restoring. It is the disaster-recovery case the
	// backup exists for, and it keeps one server on the node at a time: two
	// ScyllaDB instances exhaust the host-wide fs.aio-max-nr between them.
	require.NoError(t, stopPod(t.Context(), infra.cli, ns, "db"))

	// Restore into a fresh claim and boot the same image on it. A backup that
	// cannot be booted is not a backup.
	require.NoError(t, provisionDatabaseClaim(t.Context(), infra.cli, ns, "restored-pvc", place))

	restore := archiveRestore(t, infra.backupTestInfra, ns, "restored-pvc", archiveFile)
	require.NoError(t, pvmigrate.RunRestore(t.Context(), restore))

	require.NoError(t, startDatabaseOnClaim(t.Context(), infra.cli, ns, "restored-pvc", "restored-db", db, place))
	waitForDatabase(t, infra, ns, "restored-db", db)

	restored := databaseCount(t, infra, ns, "restored-db", db)
	assert.Equal(t, seeded, restored,
		"the restored database should hold what was committed when the snapshot was cut")
}

// assertFlushRefused runs a backup whose flush has to fail, and checks it
// failed without quoting the password and left the database writable with no
// snapshot or clone behind.
func assertFlushRefused(
	t *testing.T, infra *snapshotInfra, ns string, cred credentialCase, name string,
	backup pvmigrate.Backup, wantErr string,
) {
	t.Helper()

	err := pvmigrate.RunBackup(t.Context(), backup)
	require.Error(t, err, name)
	assert.NotContains(t, err.Error(), cred.password, "%s: the error must not quote the password", name)

	if wantErr != "" {
		assert.Contains(t, err.Error(), wantErr, name)
	}

	_, err = execInPod(t.Context(), infra.cli, ns, "db", cred.db.insertCmd)
	require.NoError(t, err, "%s: the database must be writable after a refused flush", name)

	eventuallyGone(t, func() []string { return listSnapshots(t, infra, ns) },
		name+": a refused flush must not leave a snapshot")
	eventuallyGone(t, func() []string { return clonesIn(t, infra, ns) },
		name+": a refused flush must not leave a clone")
}

// testFlushMongoLeakedLock covers the guard that a lock counted on the server
// needs: a lock left behind by an earlier run must fail the backup rather than
// leave the database blocked while reporting success.
//
//nolint:thelper // subtest implementation, not a helper
func testFlushMongoLeakedLock(t *testing.T, infra *snapshotInfra) {
	db := mongoDatabase("mongo:8.0")
	place := newPlacement(t, infra.cli)
	ns := newTestNS(t, infra.cli, "pvmig-leak")

	require.NoError(t, provisionDatabase(t.Context(), infra.cli, ns, "db-pvc", "db", db, place))
	waitForDatabase(t, infra, ns, "db", db)
	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")

	// Leave a lock behind, as an interrupted earlier run would.
	_, err := execInPod(t.Context(), infra.cli, ns, "db", mongoClient+` --quiet --eval 'db.fsyncLock()'`)
	require.NoError(t, err)

	backup := snapshotBackup(t, infra, ns, "archive-pvc:/leak.tar.zst")
	backup.PVC.Name = "db-pvc"
	backup.Flush = db.kind
	backup.IgnoreMounted = false
	backup.NoCleanupOnFailure = false

	err = pvmigrate.RunBackup(t.Context(), backup)
	require.Error(t, err, "a lock leaked by an earlier run must fail the backup")
	assert.Contains(t, err.Error(), "still locked")

	// Leave the database usable for whatever runs next in this namespace.
	_, err = execInPod(t.Context(), infra.cli, ns, "db",
		mongoClient+` --quiet --eval 'while(db.fsyncUnlock().lockCount>0){}'`)
	require.NoError(t, err)
}

// testFlushUnknownKind pins that an unsupported kind is refused before the
// cluster is touched, naming the kinds that do exist.
//
//nolint:thelper // subtest implementation, not a helper
func testFlushUnknownKind(t *testing.T, infra *snapshotInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra.backupTestInfra, place, archiveSeedCmd())

	backup := snapshotBackup(t, infra, ns, "archive-pvc:/x.tar.zst")
	backup.Flush = "db2"
	backup.NoCleanupOnFailure = false

	err := pvmigrate.RunBackup(t.Context(), backup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported --flush "db2"`)
	assert.Contains(t, err.Error(), "mysql", "the error should name the kinds that do exist")
}

// testFlushCommandOverrideRefused pins that a kind running more than one
// command refuses a replacement for only the first, which would otherwise lock
// with the user's client and unlock with the built-in one.
//
//nolint:thelper // subtest implementation, not a helper
func testFlushCommandOverrideRefused(t *testing.T, infra *snapshotInfra) {
	place := newPlacement(t, infra.cli)
	ns := seedPlacedPVC(t, infra.backupTestInfra, place, archiveSeedCmd())
	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")

	backup := snapshotBackup(t, infra, ns, "archive-pvc:/x.tar.zst")
	backup.Flush = "mongodb"
	backup.FlushCommand = []string{"mongosh", "--eval", "db.fsyncLock()"}
	backup.NoCleanupOnFailure = false

	err := pvmigrate.RunBackup(t.Context(), backup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runs three")
}

// stopPod deletes a pod and waits for it to go, so whatever it held, from a
// claim to a kernel resource, is actually free before the next one starts.
func stopPod(ctx context.Context, cli *k8s.ClusterClient, ns, name string) error {
	if err := cli.KubeClient.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete pod %s/%s: %w", ns, name, err)
	}

	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
		_, err := cli.KubeClient.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("waiting for pod %s/%s to go: %w", ns, name, err)
		}

		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("pod %s/%s did not go away: %w", ns, name, context.DeadlineExceeded)
}

// databaseCount returns the row or document count as the database reports it.
func databaseCount(t *testing.T, infra *snapshotInfra, ns, pod string, db database) string {
	t.Helper()

	out, err := execInPod(t.Context(), infra.cli, ns, pod, db.countCmd)
	require.NoError(t, err)

	return strings.TrimSpace(out)
}

// waitForDatabase blocks until the server answers, since a pod that is running
// is not yet a database that accepts connections. On a timeout it reports what
// the server itself said, because "never became ready" on its own sends the
// reader looking in the wrong place: ScyllaDB, for one, aborts on a host-wide
// kernel limit (fs.aio-max-nr) that other instances on the node can exhaust,
// and that is only visible in its log.
func waitForDatabase(t *testing.T, infra *snapshotInfra, ns, pod string, db database) {
	t.Helper()

	ready := false

	for deadline := time.Now().Add(databaseReadyTimeout); time.Now().Before(deadline); {
		if _, err := execInPod(t.Context(), infra.cli, ns, pod, db.probeCmd); err == nil {
			ready = true

			break
		}

		time.Sleep(5 * time.Second)
	}

	if !ready {
		t.Fatalf("database %s in pod %s never became ready within %s; its last log lines:\n%s",
			db.kind, pod, databaseReadyTimeout, tailPodLog(t, infra, ns, pod))
	}
}

// tailPodLog returns the end of a pod's log, for a failure message that would
// otherwise say only that something did not happen.
func tailPodLog(t *testing.T, infra *snapshotInfra, ns, pod string) string {
	t.Helper()

	lines := int64(20)

	stream, err := infra.cli.KubeClient.CoreV1().Pods(ns).
		GetLogs(pod, &corev1.PodLogOptions{TailLines: &lines}).Stream(t.Context())
	if err != nil {
		return "could not read the pod log: " + err.Error()
	}

	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		return "could not read the pod log: " + err.Error()
	}

	return string(data)
}

// provisionDatabaseClaim creates a claim big enough for a database, which the
// 64Mi the other helpers use is not.
func provisionDatabaseClaim(ctx context.Context, cli *k8s.ClusterClient, ns, pvcName string, place placement) error {
	storageClassRef := &place.class

	claim := corev1.PersistentVolumeClaim{
		Name:      pvcName,
		Namespace: ns,
		Labels:    resourceLabels,
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: storageClassRef,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{"storage": resource.MustParse("2Gi")},
			},
		},
	}

	if _, err := cli.KubeClient.CoreV1().PersistentVolumeClaims(ns).
		Create(ctx, &claim, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create database PVC %s/%s: %w", ns, pvcName, err)
	}

	return nil
}

// provisionDatabase creates the claim and starts the server on it.
func provisionDatabase(
	ctx context.Context, cli *k8s.ClusterClient, ns, pvcName, podName string, db database, place placement,
) error {
	if err := provisionDatabaseClaim(ctx, cli, ns, pvcName, place); err != nil {
		return err
	}

	return startDatabaseOnClaim(ctx, cli, ns, pvcName, podName, db, place)
}

// startDatabaseOnClaim boots the server image on an existing claim, which is
// how a restored volume is checked: the same image, the same data path.
func startDatabaseOnClaim(
	ctx context.Context, cli *k8s.ClusterClient, ns, pvcName, podName string, db database, place placement,
) error {
	grace := int64(0)

	pod := corev1.Pod{
		Name:      podName,
		Namespace: ns,
		Labels:    resourceLabels,
		Spec: corev1.PodSpec{
			NodeSelector:                  map[string]string{corev1.LabelHostname: place.node},
			TerminationGracePeriodSeconds: &grace,
			SecurityContext:               &corev1.PodSecurityContext{FSGroup: db.fsGroup},
			Volumes: []corev1.Volume{{
				Name:                  "data",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
			}},
			Containers: []corev1.Container{{
				Name:         "db",
				Image:        db.image,
				Args:         db.args,
				Env:          db.env,
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: db.dataPath, SubPath: db.subPath}},
			}},
		},
	}

	if _, err := cli.KubeClient.CoreV1().Pods(ns).Create(ctx, &pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create database pod %s/%s: %w", ns, podName, err)
	}

	// A database image can take minutes to pull on a node that has not
	// run it before.
	return waitPodRunningWithin(ctx, cli, ns, podName, databaseReadyTimeout)
}
