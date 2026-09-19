package flush

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilexec "k8s.io/client-go/util/exec"
)

// newTestSession builds a Session around a pipe, standing in for the exec
// stream, so the marker scan and the end-of-session paths can be driven
// without a cluster.
func newTestSession() (*Session, *io.PipeReader) {
	reader, writer := io.Pipe()
	_, cancel := context.WithCancel(context.Background())

	return &Session{stdin: writer, done: make(chan error, 1), cancel: cancel}, reader
}

func TestWaitFor_FindsMarkerInOutput(t *testing.T) {
	t.Parallel()

	session, _ := newTestSession()
	out := &lockedWriter{session: session}

	go func() {
		time.Sleep(50 * time.Millisecond)

		_, _ = out.Write([]byte("mysql> pv-migrate-quiesced\n"))
	}()

	require.NoError(t, session.WaitFor(context.Background(), "pv-migrate-quiesced", 2*time.Second))
}

func TestWaitFor_TimesOutWithOutput(t *testing.T) {
	t.Parallel()

	session, _ := newTestSession()
	out := &lockedWriter{session: session}
	_, _ = out.Write([]byte("ERROR 1227 (42000): Access denied; you need BACKUP_ADMIN\n"))

	err := session.WaitFor(context.Background(), "pv-migrate-quiesced", 300*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Contains(t, err.Error(), "BACKUP_ADMIN", "the client's own output has to reach the operator")
}

func TestWaitFor_SessionEndedFirst(t *testing.T) {
	t.Parallel()

	session, _ := newTestSession()
	out := &lockedWriter{session: session}
	_, _ = out.Write([]byte("connection refused\n"))

	session.done <- utilexec.CodeExitError{Err: errors.New("exit"), Code: 1}

	err := session.WaitFor(context.Background(), "pv-migrate-quiesced", 2*time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session ended before")
	assert.Contains(t, err.Error(), "exited with code 1")
	assert.Contains(t, err.Error(), "connection refused")
}

func TestWaitFor_ContextCancelled(t *testing.T) {
	t.Parallel()

	session, _ := newTestSession()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := session.WaitFor(ctx, "never", 2*time.Second)
	require.ErrorIs(t, err, context.Canceled)
}

func TestWrite_AfterSessionEnded(t *testing.T) {
	t.Parallel()

	session, _ := newTestSession()
	session.done <- errors.New("stream closed")

	err := session.Write("UNLOCK INSTANCE;")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session ended before")
	assert.Contains(t, err.Error(), "UNLOCK INSTANCE;")
}

func TestWrite_ReachesStdinWithNewline(t *testing.T) {
	t.Parallel()

	session, reader := newTestSession()

	got := make(chan string, 1)

	go func() {
		buf := make([]byte, 64)

		n, _ := reader.Read(buf)
		got <- string(buf[:n])
	}()

	require.NoError(t, session.Write("LOCK INSTANCE FOR BACKUP;"))
	assert.Equal(t, "LOCK INSTANCE FOR BACKUP;\n", <-got)
}

func TestClose_ReturnsExitError(t *testing.T) {
	t.Parallel()

	session, reader := newTestSession()

	go func() {
		_, _ = io.ReadAll(reader)

		session.done <- utilexec.CodeExitError{Err: errors.New("exit"), Code: 2}
	}()

	err := session.Close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exited with code 2")
}

func TestClose_CleanExit(t *testing.T) {
	t.Parallel()

	session, reader := newTestSession()

	go func() {
		_, _ = io.ReadAll(reader)

		session.done <- nil
	}()

	require.NoError(t, session.Close())
}

func TestExitError(t *testing.T) {
	t.Parallel()

	require.NoError(t, exitError(nil))
	require.EqualError(t, exitError(utilexec.CodeExitError{Err: errors.New("x"), Code: 7}), "client exited with code 7")
	require.EqualError(t, exitError(errors.New("plain")), "plain")
}
