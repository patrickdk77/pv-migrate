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
	// args are the server's own start-up arguments, which only ScyllaDB needs
	// in order to fit on a test node.
	args      []string
	env       []corev1.EnvVar
	probeCmd  string
	seedCmd   string
	countCmd  string
	insertCmd string
}

func mysqlDatabase() database {
	return database{
		kind:     "mysql",
		image:    "mysql:8.4",
		dataPath: "/var/lib/mysql",
		env: []corev1.EnvVar{
			{Name: "MYSQL_ROOT_PASSWORD", Value: "pvmtest"},
			{Name: "MYSQL_DATABASE", Value: "app"},
		},
		probeCmd: `MYSQL_PWD=$MYSQL_ROOT_PASSWORD mysql -uroot --batch --skip-column-names -e 'SELECT 1'`,
		seedCmd: `MYSQL_PWD=$MYSQL_ROOT_PASSWORD mysql -uroot app --batch --skip-column-names -e ` +
			`'CREATE TABLE IF NOT EXISTS t(id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(64)); ` +
			`INSERT INTO t(v) VALUES ("seeded")'`,
		countCmd: `MYSQL_PWD=$MYSQL_ROOT_PASSWORD mysql -uroot app --batch --skip-column-names ` +
			`-e 'SELECT COUNT(*) FROM t'`,
		insertCmd: `MYSQL_PWD=$MYSQL_ROOT_PASSWORD mysql -uroot app --batch --skip-column-names ` +
			`-e 'INSERT INTO t(v) VALUES ("after-backup")'`,
	}
}

func mariadbDatabase() database {
	return database{
		kind:     "mariadb",
		image:    "mariadb:11.4",
		dataPath: "/var/lib/mysql",
		env: []corev1.EnvVar{
			{Name: "MARIADB_ROOT_PASSWORD", Value: "pvmtest"},
			{Name: "MARIADB_DATABASE", Value: "app"},
		},
		probeCmd: `MYSQL_PWD=$MARIADB_ROOT_PASSWORD mariadb -uroot --batch --skip-column-names -e 'SELECT 1'`,
		seedCmd: `MYSQL_PWD=$MARIADB_ROOT_PASSWORD mariadb -uroot app --batch --skip-column-names -e ` +
			`'CREATE TABLE IF NOT EXISTS t(id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(64)); ` +
			`INSERT INTO t(v) VALUES ("seeded")'`,
		countCmd: `MYSQL_PWD=$MARIADB_ROOT_PASSWORD mariadb -uroot app --batch --skip-column-names ` +
			`-e 'SELECT COUNT(*) FROM t'`,
		insertCmd: `MYSQL_PWD=$MARIADB_ROOT_PASSWORD mariadb -uroot app --batch --skip-column-names ` +
			`-e 'INSERT INTO t(v) VALUES ("after-backup")'`,
	}
}

func postgresDatabase() database {
	return database{
		kind:     "postgres",
		image:    "postgres:18",
		dataPath: "/var/lib/postgresql/data",
		env: []corev1.EnvVar{
			{Name: "POSTGRES_PASSWORD", Value: "pvmtest"},
			{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
		},
		probeCmd: `psql -U postgres -tAc 'SELECT 1'`,
		seedCmd: `psql -U postgres -q ` +
			`-c 'CREATE TABLE IF NOT EXISTS t(id serial primary key, v text)' ` +
			`-c "INSERT INTO t(v) VALUES ('seeded')"`,
		countCmd:  `psql -U postgres -tAc 'SELECT COUNT(*) FROM t'`,
		insertCmd: `psql -U postgres -q -c "INSERT INTO t(v) VALUES ('after-backup')"`,
	}
}

func mongoDatabase() database {
	return database{
		kind:      "mongodb",
		image:     "mongo:8.0",
		dataPath:  "/data/db",
		probeCmd:  `mongosh --quiet --eval 'db.adminCommand("ping").ok'`,
		seedCmd:   `mongosh --quiet --eval 'db.t.insertOne({v:"seeded"})'`,
		countCmd:  `mongosh --quiet --eval 'print(db.t.countDocuments())'`,
		insertCmd: `mongosh --quiet --eval 'db.t.insertOne({v:"after-backup"})'`,
	}
}

func scyllaDatabase() database {
	return database{
		kind:     "scylladb",
		image:    "scylladb/scylla:6.2",
		dataPath: "/var/lib/scylla",
		args:     []string{"--smp", "1", "--memory", "1G", "--overprovisioned", "1", "--developer-mode", "1"},
		probeCmd: `nodetool status | grep -q '^UN'`,
		seedCmd: `cqlsh -e "CREATE KEYSPACE IF NOT EXISTS app WITH replication=` +
			`{'class':'SimpleStrategy','replication_factor':1}; ` +
			`CREATE TABLE IF NOT EXISTS app.t(id int PRIMARY KEY, v text); ` +
			`INSERT INTO app.t(id,v) VALUES (1,'seeded');"`,
		countCmd:  `cqlsh -e "SELECT COUNT(*) FROM app.t" | sed -n '4p' | tr -d ' '`,
		insertCmd: `cqlsh -e "INSERT INTO app.t(id,v) VALUES (2,'after-backup')"`,
	}
}

func TestFlush(t *testing.T) {
	t.Parallel()

	infra := setupSnapshotInfra(t)

	kinds := []database{
		mysqlDatabase(), mariadbDatabase(), postgresDatabase(), mongoDatabase(), scyllaDatabase(),
	}

	for _, db := range kinds {
		t.Run(db.kind, func(t *testing.T) {
			t.Parallel()
			testFlushRoundTrip(t, infra, db)
		})
	}

	t.Run("MongoLeakedLockIsCaught", func(t *testing.T) {
		t.Parallel()
		testFlushMongoLeakedLock(t, infra)
	})

	t.Run("EveryKindIsCovered", func(t *testing.T) {
		t.Parallel()

		covered := make([]string, 0, len(kinds))
		for _, db := range kinds {
			covered = append(covered, db.kind)
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

// testFlushMongoLeakedLock covers the guard that a lock counted on the server
// needs: a lock left behind by an earlier run must fail the backup rather than
// leave the database blocked while reporting success.
//
//nolint:thelper // subtest implementation, not a helper
func testFlushMongoLeakedLock(t *testing.T, infra *snapshotInfra) {
	db := mongoDatabase()
	place := newPlacement(t, infra.cli)
	ns := newTestNS(t, infra.cli, "pvmig-leak")

	require.NoError(t, provisionDatabase(t.Context(), infra.cli, ns, "db-pvc", "db", db, place))
	waitForDatabase(t, infra, ns, "db", db)
	placePod(t, infra.backupTestInfra, place, ns, "archive-pvc", "archive-pod")

	// Leave a lock behind, as an interrupted earlier run would.
	_, err := execInPod(t.Context(), infra.cli, ns, "db", `mongosh --quiet --eval 'db.fsyncLock()'`)
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
		`mongosh --quiet --eval 'while(db.fsyncUnlock().lockCount>0){}'`)
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
			Volumes: []corev1.Volume{{
				Name:                  "data",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
			}},
			Containers: []corev1.Container{{
				Name:         "db",
				Image:        db.image,
				Args:         db.args,
				Env:          db.env,
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: db.dataPath}},
			}},
		},
	}

	if _, err := cli.KubeClient.CoreV1().Pods(ns).Create(ctx, &pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create database pod %s/%s: %w", ns, podName, err)
	}

	return waitPodRunning(ctx, cli, ns, podName)
}
