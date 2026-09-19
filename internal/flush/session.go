// Package flush quiesces a database for the moment a snapshot is cut.
//
// The lock a database offers for this dies with the session that took it, so
// three separate execs to lock, snapshot and unlock would all succeed while the
// snapshot ran unprotected. A Session is one exec'd client process in the
// database's own pod, held open on its stdin across the cut, so the lock lives
// exactly as long as it needs to and not a moment longer.
package flush

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// markerPollInterval is how often the client's output is checked for a marker.
const markerPollInterval = 200 * time.Millisecond

// Session is one client process exec'd in a pod, with its stdin held open.
type Session struct {
	stdin  io.WriteCloser
	done   chan error
	mu     sync.Mutex
	output bytes.Buffer
	cancel context.CancelFunc
}

// Open execs command in the container and returns a Session whose stdin is
// still open. The process keeps running until Close.
func Open(
	ctx context.Context,
	restConfig *rest.Config,
	kube kubernetes.Interface,
	namespace, pod, container string,
	command []string,
) (*Session, error) {
	req := kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			// A TTY would echo input and translate newlines, neither of which
			// a marker scan can tolerate.
			TTY: false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("failed to create exec into %s/%s: %w", namespace, pod, err)
	}

	reader, writer := io.Pipe()
	streamCtx, cancel := context.WithCancel(ctx)

	session := &Session{stdin: writer, done: make(chan error, 1), cancel: cancel}

	go func() {
		streamErr := executor.StreamWithContext(streamCtx, remotecommand.StreamOptions{
			Stdin:  reader,
			Stdout: &lockedWriter{session: session},
			Stderr: &lockedWriter{session: session},
		})

		// Unblock a writer still waiting on the pipe if the process died.
		_ = reader.CloseWithError(errors.New("session ended"))

		session.done <- streamErr
	}()

	return session, nil
}

// Write sends one statement to the client, terminated by a newline.
func (s *Session) Write(statement string) error {
	select {
	case err := <-s.done:
		s.done <- err

		return fmt.Errorf("session ended before %q could be sent: %w", statement, exitError(err))
	default:
	}

	if _, err := io.WriteString(s.stdin, statement+"\n"); err != nil {
		return fmt.Errorf("failed to send %q to the session: %w", statement, err)
	}

	return nil
}

// WaitFor blocks until marker appears in the client's output, which is how a
// caller learns a statement it sent has actually executed rather than merely
// been buffered. A session that ends first is an error carrying its output.
func (s *Session) WaitFor(ctx context.Context, marker string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	tick := time.NewTicker(markerPollInterval)
	defer tick.Stop()

	for {
		if s.sawMarker(marker) {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out after %s waiting for %q; output so far: %s",
				timeout, marker, s.Output())
		case err := <-s.done:
			s.done <- err

			return fmt.Errorf("session ended before %q appeared: %w; output: %s",
				marker, exitError(err), s.Output())
		case <-tick.C:
		}
	}
}

// Close ends the session by closing its stdin, which makes a well-behaved
// client exit, and waits for it. The process's exit error is returned, so a
// client that failed after the unlock is still reported.
func (s *Session) Close() error {
	_ = s.stdin.Close()

	err := <-s.done
	s.done <- err

	s.cancel()

	return exitError(err)
}

// Output returns everything the client has printed so far.
func (s *Session) Output() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return strings.TrimSpace(s.output.String())
}

func (s *Session) sawMarker(marker string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return bytes.Contains(s.output.Bytes(), []byte(marker))
}

// exitError keeps a process's exit code readable in the error, since the
// exec's own error only names it as an exit status.
func exitError(err error) error {
	if err == nil {
		return nil
	}

	if codeErr, ok := errors.AsType[utilexec.CodeExitError](err); ok {
		return fmt.Errorf("client exited with code %d", codeErr.Code)
	}

	return err
}

type lockedWriter struct {
	session *Session
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.session.mu.Lock()
	defer w.session.mu.Unlock()

	return w.session.output.Write(data)
}

// Run execs command, waits for it to exit, and returns what it printed. It is
// a Session opened and closed at once, for a quiesce that holds no state.
func Run(
	ctx context.Context,
	restConfig *rest.Config,
	kube kubernetes.Interface,
	namespace, pod, container string,
	command []string,
) (string, error) {
	session, err := Open(ctx, restConfig, kube, namespace, pod, container, command)
	if err != nil {
		return "", err
	}

	err = session.Close()

	return session.Output(), err
}
