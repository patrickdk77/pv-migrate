package flush_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/utkuozdemir/pv-migrate/internal/flush"
)

// fakeClient prints which client it is, its arguments one per line, the
// password variables it inherited, and then everything that reached its
// stdin, so a run shows where the password went and what followed it.
const fakeClient = `#!/bin/sh
echo "CLIENT=$(basename "$0")"
for arg in "$@"; do echo "ARG=$arg"; done
echo "MYSQL_PWD=${MYSQL_PWD-<unset>}"
echo "PGPASSWORD=${PGPASSWORD-<unset>}"
echo "STDIN:"
cat
`

// shells are the /bin/sh the database images actually carry: dash in the
// Debian-based postgres, mongo and mariadb images, bash in the Oracle Linux
// based mysql and percona images. The scripts have to be POSIX for both.
func shells(t *testing.T) []string {
	t.Helper()

	var found []string

	for _, name := range []string{"dash", "bash"} {
		if path, err := exec.LookPath(name); err == nil {
			found = append(found, path)
		}
	}

	if len(found) == 0 {
		t.Skip("neither dash nor bash to run the scripts with")
	}

	return found
}

// toolDir holds the named fake clients and a cat for them to use, and nothing
// else, so a real client installed on the test machine can never be the one
// that runs.
func toolDir(t *testing.T, clients ...string) string {
	t.Helper()

	dir := t.TempDir()

	for _, name := range clients {
		//nolint:gosec // it has to be executable for the shell to find it
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(fakeClient), 0o700))
	}

	for _, tool := range []string{"cat", "basename"} {
		path, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(path, filepath.Join(dir, tool)))
	}

	return dir
}

type run struct {
	out  string
	args []string
	env  map[string]string
	code int
}

// runScript runs argv, which starts with "sh -c SCRIPT", under shell, with
// stdin and the given environment, and splits what the fake printed.
func runScript(t *testing.T, shell, dir string, argv []string, stdin string, env ...string) run {
	t.Helper()

	require.Equal(t, "sh", argv[0], "the built-in commands are sh -c scripts")

	//nolint:gosec // running the script under a chosen shell is the test
	cmd := exec.CommandContext(t.Context(), shell, argv[1:]...)

	cmd.Env = append([]string{"PATH=" + dir}, env...)
	cmd.Stdin = strings.NewReader(stdin)

	out, err := cmd.CombinedOutput()

	result := run{out: string(out), env: map[string]string{}}

	if exitErr, ok := err.(*exec.ExitError); ok { //nolint:errorlint // only the exit code is wanted
		result.code = exitErr.ExitCode()
	} else {
		require.NoError(t, err)
	}

	head, _, _ := strings.Cut(string(out), "STDIN:")
	for line := range strings.SplitSeq(head, "\n") {
		if arg, ok := strings.CutPrefix(line, "ARG="); ok {
			result.args = append(result.args, arg)
		} else if key, value, ok := strings.Cut(line, "="); ok {
			result.env[key] = value
		}
	}

	return result
}

func stdinOf(got run) string {
	_, after, _ := strings.Cut(got.out, "STDIN:\n")

	return after
}

func mysqlCommand(t *testing.T, user string) []string {
	t.Helper()

	spec, err := flush.Lookup("mysql")
	require.NoError(t, err)

	return spec.Command(user)
}

func skipOnWindows(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the scripts run in the database container's shell")
	}
}

// The password arrives on the first line of stdin and leaves as MYSQL_PWD,
// never as an argument, and every line after it reaches the client untouched.
func TestMySQLScript_PasswordGoesToTheEnvironmentNotTheCommandLine(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	for _, shell := range shells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			t.Parallel()

			got := runScript(t, shell, toolDir(t, "mysql"), mysqlCommand(t, "backup"),
				"s3cret pass\nSELECT 1;\nSELECT 2;\n")

			assert.Equal(t, "s3cret pass", got.env["MYSQL_PWD"])
			assert.NotContains(t, strings.Join(got.args, " "), "s3cret", "the password must not be an argument")
			assert.Contains(t, got.args, "-ubackup")
			assert.Equal(t, "SELECT 1;\nSELECT 2;\n", stdinOf(got),
				"reading the password must take exactly one line and leave the statements")
		})
	}
}

// MySQL images and MariaDB 10.3 ship only mysql; MariaDB 11.4 onwards only
// mariadb; 10.4 to 10.11 both. Whichever exists is used.
func TestMySQLScript_UsesWhicheverClientTheImageHas(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	for clients, want := range map[string]string{
		"mysql":         "mysql",
		"mariadb":       "mariadb",
		"mysql,mariadb": "mariadb",
	} {
		for _, shell := range shells(t) {
			t.Run(clients+"/"+filepath.Base(shell), func(t *testing.T) {
				t.Parallel()

				got := runScript(t, shell, toolDir(t, strings.Split(clients, ",")...), mysqlCommand(t, ""), "\n")
				assert.Equal(t, want, got.env["CLIENT"])
			})
		}
	}
}

func TestMySQLScript_NoClientIsAClearError(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	for _, shell := range shells(t) {
		got := runScript(t, shell, toolDir(t), mysqlCommand(t, ""), "\n")
		assert.Equal(t, 127, got.code)
		assert.Contains(t, got.out, "neither a mariadb nor a mysql client")
	}
}

// With no password given, the one the image seeded is used, MariaDB's own
// variable first since MariaDB images accept both.
func TestMySQLScript_FallsBackToWhatTheImageSeeded(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"mariadb variable first": {[]string{"MARIADB_ROOT_PASSWORD=m", "MYSQL_ROOT_PASSWORD=r"}, "m"},
		"mysql variable":         {[]string{"MYSQL_ROOT_PASSWORD=r"}, "r"},
		"MYSQL_PWD already set":  {[]string{"MYSQL_PWD=p", "MYSQL_ROOT_PASSWORD=r"}, "p"},
		"a given password wins":  {[]string{"MYSQL_ROOT_PASSWORD=r"}, "given"},
	} {
		for _, shell := range shells(t) {
			t.Run(name+"/"+filepath.Base(shell), func(t *testing.T) {
				t.Parallel()

				stdin := "\n"
				if tc.want == "given" {
					stdin = "given\n"
				}

				got := runScript(t, shell, toolDir(t, "mysql"), mysqlCommand(t, ""), stdin, tc.env...)
				assert.Equal(t, tc.want, got.env["MYSQL_PWD"])
			})
		}
	}
}

func TestMySQLScript_DefaultUserIsRoot(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	got := runScript(t, shells(t)[0], toolDir(t, "mysql"), mysqlCommand(t, ""), "\n")
	assert.Contains(t, got.args, "-uroot")
}

// A user name is an argument of its own, so nothing in it is run.
func TestMySQLScript_UserIsNeverInterpreted(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	got := runScript(t, shells(t)[0], toolDir(t, "mysql"), mysqlCommand(t, "x; touch pwned $(id)"), "\n")
	assert.Contains(t, got.args, "-ux; touch pwned $(id)")
}

func postgresCommand(t *testing.T, user string) []string {
	t.Helper()

	spec, err := flush.Lookup("postgres")
	require.NoError(t, err)

	return spec.Command(user)
}

func TestPostgresScript(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	for _, shell := range shells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			t.Parallel()

			dir := toolDir(t, "psql")

			got := runScript(t, shell, dir, postgresCommand(t, "backup"), "pgpw\n")
			assert.Equal(t, "pgpw", got.env["PGPASSWORD"])
			assert.NotContains(t, strings.Join(got.args, " "), "pgpw")
			assert.Contains(t, strings.Join(got.args, " "), "-U backup")
			assert.Contains(t, strings.Join(got.args, " "), "-d postgres",
				"a user given with --flush-user often has no database of its own name")
			assert.Contains(t, got.args, "CHECKPOINT")
			assert.Contains(t, got.args, "-w", "a missing password must fail, not prompt")

			// No password: PGPASSWORD stays unset, since the image trusts
			// local connections and an empty one would still be tried.
			got = runScript(t, shell, dir, postgresCommand(t, ""), "\n", "POSTGRES_USER=app")
			assert.Equal(t, "<unset>", got.env["PGPASSWORD"])
			assert.Contains(t, strings.Join(got.args, " "), "-U app")

			// No password given but the image seeded one: use it, for an
			// image whose local connections need a password.
			got = runScript(t, shell, dir, postgresCommand(t, ""), "\n", "POSTGRES_PASSWORD=seeded")
			assert.Equal(t, "seeded", got.env["PGPASSWORD"])

			got = runScript(t, shell, dir, postgresCommand(t, "backup"), "given\n", "POSTGRES_PASSWORD=seeded")
			assert.Equal(t, "given", got.env["PGPASSWORD"], "a given password wins")

			got = runScript(t, shell, dir, postgresCommand(t, ""), "\n")
			assert.Contains(t, strings.Join(got.args, " "), "-U postgres")
		})
	}
}

func mongoCommand(t *testing.T, user string, release bool) []string {
	t.Helper()

	spec, err := flush.Lookup("mongodb")
	require.NoError(t, err)

	if release {
		return spec.Release(user)
	}

	return spec.Command(user)
}

func TestMongoScript(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	for _, shell := range shells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			t.Parallel()

			lockJS := lastArg(mongoCommand(t, "", false))
			releaseJS := lastArg(mongoCommand(t, "", true))

			// mongo 4.4 ships only the legacy shell, 6.0 onwards only mongosh.
			got := runScript(t, shell, toolDir(t, "mongo"), mongoCommand(t, "backup", false), "mpw\n")
			assert.Equal(t, "mongo", got.env["CLIENT"])
			assert.Equal(t, []string{
				"--quiet", "-u", "backup", "-p", "mpw", "--authenticationDatabase", "admin",
				"--eval", lockJS,
			}, got.args, "the JavaScript, quotes and all, has to arrive as one argument as built")

			got = runScript(t, shell, toolDir(t, "mongosh", "mongo"), mongoCommand(t, "backup", false), "mpw\n")
			assert.Equal(t, "mongosh", got.env["CLIENT"], "mongosh first where an image has both")

			got = runScript(t, shell, toolDir(t, "mongosh"), mongoCommand(t, "backup", true), "mpw\n")
			assert.Equal(t, releaseJS, got.args[len(got.args)-1])

			// Nothing given and nothing seeded: connect without -u, since an
			// empty one breaks the connection.
			got = runScript(t, shell, toolDir(t, "mongosh"), mongoCommand(t, "", false), "\n")
			assert.Equal(t, []string{"--quiet", "--eval", lockJS}, got.args)

			// Nothing given but the image seeded a root user: use it.
			got = runScript(t, shell, toolDir(t, "mongosh"), mongoCommand(t, "", false), "\n",
				"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=seeded")
			assert.Contains(t, got.args, "root")
			assert.Contains(t, got.args, "seeded")
		})
	}
}

func lastArg(argv []string) string {
	return argv[len(argv)-1]
}

func TestMongoScript_NoClientIsAClearError(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	got := runScript(t, shells(t)[0], toolDir(t), mongoCommand(t, "", false), "\n")
	assert.Equal(t, 127, got.code)
	assert.Contains(t, got.out, "neither mongosh nor mongo")
}
