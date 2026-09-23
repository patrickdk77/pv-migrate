package bucketstorage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/utkuozdemir/pv-migrate/internal/flush"
	"github.com/utkuozdemir/pv-migrate/internal/k8s"
	"github.com/utkuozdemir/pv-migrate/internal/pvc"
	"github.com/utkuozdemir/pv-migrate/internal/rclone"
)

func TestUsesSnapshot(t *testing.T) {
	t.Parallel()

	assert.False(t, usesSnapshot(&Request{}))
	assert.True(t, usesSnapshot(&Request{Snapshot: true}))
	assert.True(t, usesSnapshot(&Request{FromSnapshot: "existing"}))
	assert.True(t, usesSnapshot(&Request{Flush: "mysql"}), "--flush implies a snapshot")
}

func TestValidateSnapshotRequest_OK(t *testing.T) {
	t.Parallel()

	for name, req := range map[string]Request{
		"plain run":         {Direction: rclone.DirectionBackup},
		"snapshot":          {Direction: rclone.DirectionBackup, Snapshot: true},
		"from snapshot":     {Direction: rclone.DirectionBackup, FromSnapshot: "snap"},
		"flush":             {Direction: rclone.DirectionBackup, Flush: "mysql"},
		"restore, no flags": {Direction: rclone.DirectionRestore},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, validateSnapshotRequest(&req))
		})
	}
}

func TestValidateSnapshotRequest_Errors(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		req     Request
		wantErr string
	}{
		"snapshot on restore": {
			req:     Request{Direction: rclone.DirectionRestore, Snapshot: true},
			wantErr: "apply to a backup, not a restore",
		},
		"flush on restore": {
			req:     Request{Direction: rclone.DirectionRestore, Flush: "mysql"},
			wantErr: "apply to a backup, not a restore",
		},
		"flush with from-snapshot": {
			req:     Request{Direction: rclone.DirectionBackup, Flush: "mysql", FromSnapshot: "snap"},
			wantErr: "cannot be combined with --from-snapshot",
		},
		"unknown flush kind": {
			req:     Request{Direction: rclone.DirectionBackup, Flush: "db2"},
			wantErr: `unsupported --flush "db2"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := validateSnapshotRequest(&tc.req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// A kept snapshot must not carry the instance label, or the cleanup that
// removes the run's other leftovers takes it too.
func TestSnapshotLabels(t *testing.T) {
	t.Parallel()

	kept := snapshotLabels("rel", true)
	assert.NotContains(t, kept, "app.kubernetes.io/instance")
	assert.Equal(t, "pv-migrate", kept["app.kubernetes.io/name"])

	removable := snapshotLabels("rel", false)
	assert.Equal(t, "rel", removable["app.kubernetes.io/instance"])
	assert.Equal(t, runLabels("rel"), removable)
}

func flushSource() *pvc.Info {
	return &pvc.Info{
		Claim:      &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "data"},
		MountedPod: "db-0",
	}
}

func clientWith(objects ...runtime.Object) *k8s.ClusterClient {
	return &k8s.ClusterClient{KubeClient: fake.NewClientset(objects...)}
}

// dbSecret is the Secret db-secrets in the claim's namespace.
func dbSecret(data map[string]string) *corev1.Secret {
	secret := &corev1.Secret{Name: "db-secrets", Namespace: "ns", Data: map[string][]byte{}}
	for k, v := range data {
		secret.Data[k] = []byte(v)
	}

	return secret
}

func TestPrepareFlush_RefusesCommandOverrideForPairKinds(t *testing.T) {
	t.Parallel()

	_, err := prepareFlush(t.Context(), clientWith(),
		&Request{Flush: "mongodb", FlushCommand: []string{"mongosh"}}, flushSource())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runs three")

	plan, err := prepareFlush(t.Context(), clientWith(),
		&Request{Flush: "postgres", FlushCommand: []string{"psql", "-c", "CHECKPOINT"}}, flushSource())
	require.NoError(t, err)
	assert.Equal(t, []string{"psql", "-c", "CHECKPOINT"}, plan.spec.Command(""))
	assert.Nil(t, plan.creds, "a replaced command reads no password line, so none may be sent to it")
}

func TestPrepareFlush_NeedsAMountedPod(t *testing.T) {
	t.Parallel()

	source := &pvc.Info{Claim: &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "data"}}

	_, err := prepareFlush(t.Context(), clientWith(), &Request{Flush: "mysql"}, source)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pod has claim data mounted")
}

// Every built-in script reads a password line before anything else, so one
// has to be sent even when nobody gave a password. Without it the script's
// read would take the first SQL statement as the password, and the session
// would lock nothing while the client waited for input that never came.
func TestPrepareFlush_AlwaysSendsALineToABuiltInScript(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"mysql", "mariadb", "postgres", "mongodb"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			plan, err := prepareFlush(t.Context(), clientWith(), &Request{Flush: kind}, flushSource())
			require.NoError(t, err)
			require.NotNil(t, plan.creds, "an empty line still has to reach the script's read")
			assert.Empty(t, plan.creds.User)
			assert.Empty(t, plan.creds.Password)
		})
	}
}

// nodetool logs in as nobody, so it reads nothing and nothing is sent.
func TestPrepareFlush_SendsNothingToNodetool(t *testing.T) {
	t.Parallel()

	plan, err := prepareFlush(t.Context(), clientWith(), &Request{Flush: "scylladb"}, flushSource())
	require.NoError(t, err)
	assert.Nil(t, plan.creds)
}

func TestResolveFlushCredentials_Direct(t *testing.T) {
	t.Parallel()

	plan, err := prepareFlush(t.Context(), clientWith(),
		&Request{Flush: "mysql", FlushUser: "backup", FlushPassword: "s3cret"}, flushSource())
	require.NoError(t, err)
	assert.Equal(t, "backup", plan.creds.User)
	assert.Equal(t, "s3cret", plan.creds.Password)
}

func TestResolveFlushCredentials_FromSecret(t *testing.T) {
	t.Parallel()

	secret := dbSecret(map[string]string{"password": "default-key", "root": "named-key"})

	for ref, want := range map[string]string{
		"db-secrets":      "default-key",
		"db-secrets:root": "named-key",
	} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()

			plan, err := prepareFlush(t.Context(), clientWith(secret),
				&Request{Flush: "mysql", FlushUser: "root", FlushPasswordSecret: ref}, flushSource())
			require.NoError(t, err)
			assert.Equal(t, want, plan.creds.Password)
		})
	}
}

// The Secret is read from the claim's namespace, the database's own, and no
// other: the same name elsewhere must not be picked up.
func TestResolveFlushCredentials_SecretInTheClaimsNamespaceOnly(t *testing.T) {
	t.Parallel()

	elsewhere := dbSecret(map[string]string{"password": "wrong"})
	elsewhere.Namespace = "other"

	_, err := prepareFlush(t.Context(), clientWith(elsewhere),
		&Request{Flush: "mysql", FlushPasswordSecret: "db-secrets"}, flushSource())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ns/db-secrets")
}

// A missing key names the keys that do exist, and never a value.
func TestResolveFlushCredentials_MissingKeyListsKeysNotValues(t *testing.T) {
	t.Parallel()

	secret := dbSecret(map[string]string{"root": "do-not-print-me", "monitor": "nor-me"})

	_, err := prepareFlush(t.Context(), clientWith(secret),
		&Request{Flush: "mysql", FlushPasswordSecret: "db-secrets:backup"}, flushSource())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no key "backup"`)
	assert.Contains(t, err.Error(), "monitor, root")
	assert.NotContains(t, err.Error(), "do-not-print-me")
	assert.NotContains(t, err.Error(), "nor-me")
}

func TestResolveFlushCredentials_Refusals(t *testing.T) {
	t.Parallel()

	secret := dbSecret(map[string]string{"password": "x"})

	for name, tc := range map[string]struct {
		req  Request
		want string
	}{
		"both password and secret": {
			Request{Flush: "mysql", FlushPassword: "a", FlushPasswordSecret: "db-secrets"},
			"not both",
		},
		"credentials for nodetool": {
			Request{Flush: "scylladb", FlushUser: "cassandra"},
			"nodetool",
		},
		"credentials with a replaced command": {
			Request{Flush: "mysql", FlushUser: "backup", FlushCommand: []string{"mysql"}},
			"--flush-command",
		},
		"a secret name with no name": {
			Request{Flush: "mysql", FlushPasswordSecret: ":password"},
			"must be a Secret name",
		},
		"a secret name with an empty key": {
			Request{Flush: "mysql", FlushPasswordSecret: "db-secrets:"},
			"must be a Secret name",
		},
		"a secret that does not exist": {
			Request{Flush: "mysql", FlushPasswordSecret: "missing"},
			"ns/missing",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := tc.req
			_, err := prepareFlush(t.Context(), clientWith(secret), &req, flushSource())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// A line break would end the password early and hand the rest to the client
// as a statement, which it would run.
func TestResolveFlushCredentials_RefusesAPasswordWithALineBreak(t *testing.T) {
	t.Parallel()

	for _, password := range []string{"pw\nDROP DATABASE app;", "pw\r"} {
		_, err := prepareFlush(t.Context(), clientWith(),
			&Request{Flush: "mysql", FlushPassword: password}, flushSource())
		require.ErrorIs(t, err, flush.ErrMultilinePassword)
		assert.NotContains(t, err.Error(), "DROP DATABASE", "the error must not quote the password")
	}
}

// mongo 4.4 answers a refused fsyncLock with ok: 0 and still exits 0, so
// only what the lock command printed tells a lock from a refusal.
func TestCheckLockTaken(t *testing.T) {
	t.Parallel()

	spec, err := flush.Lookup("mongodb")
	require.NoError(t, err)
	require.NotEmpty(t, spec.LockMarker)

	require.NoError(t, checkLockTaken("mongodb", spec, "pv-migrate-fsynclock=1 \n"))

	for _, out := range []string{
		"pv-migrate-fsynclock=0 not authorized on admin to execute command { fsync: 1, lock: true }",
		"",
		"MongoServerError: not authorized",
	} {
		err := checkLockTaken("mongodb", spec, out)
		require.Error(t, err, out)
		assert.Contains(t, err.Error(), "refused the lock")
	}

	// A kind with no marker has nothing to read.
	require.NoError(t, checkLockTaken("x", flush.Spec{}, ""))
}

// A clone bound on first consumer has no volume yet, so it reports no
// topology of its own. Inheriting the source's is what keeps a snapshot from
// looking restorable on a node its storage can never reach.
func TestInheritTopology(t *testing.T) {
	t.Parallel()

	source := &pvc.Info{
		Claim:        &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "data"},
		AllowedNodes: []string{"node-a"},
		PinnedNodes:  []string{"node-a"},
	}

	t.Run("an unbound clone takes the source's", func(t *testing.T) {
		t.Parallel()

		clone := &pvc.Info{Claim: &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "clone"}}
		inheritTopology(clone, source)

		assert.Equal(t, []string{"node-a"}, clone.PinnedNodes)
		assert.Equal(t, pvc.RequireNodes([]string{"node-a"}), clone.AffinityHelmValues)
	})

	t.Run("a bound clone keeps its own", func(t *testing.T) {
		t.Parallel()

		clone := &pvc.Info{
			Claim:        &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "clone"},
			AllowedNodes: []string{"node-b"},
			PinnedNodes:  []string{"node-b"},
		}
		inheritTopology(clone, source)

		assert.Equal(t, []string{"node-b"}, clone.PinnedNodes,
			"what the driver actually published beats anything inferred")
	})

	t.Run("an unconstrained source constrains nothing", func(t *testing.T) {
		t.Parallel()

		clone := &pvc.Info{Claim: &corev1.PersistentVolumeClaim{Namespace: "ns", Name: "clone"}}
		inheritTopology(clone, &pvc.Info{Claim: source.Claim})

		assert.Nil(t, clone.PinnedNodes)
	})
}
