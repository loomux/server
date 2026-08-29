package targets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultConnectTimeout = "10s"
	defaultControlPersist = "10m"
)

// RemoteExecutor runs tmux operations over SSH, using connection
// multiplexing (ControlMaster/ControlPersist) so repeated operations
// against the same target reuse one connection instead of paying a fresh
// handshake each time. Every operation shells out to the real ssh binary
// per the design spec's v1 approach.
type RemoteExecutor struct {
	host string
	user string

	port           int    // 0 means "let ssh/its config decide"
	identityFile   string // "" means "let ssh/its config decide"
	connectTimeout string
	controlPersist string
	controlPath    string
	extraArgs      []string
}

// RemoteOption configures a RemoteExecutor. Production's NewExecutor
// passes none of these — they exist for tests to point at a non-standard
// host/port/identity (e.g. a loopback sshtest server) without needing
// fields on registry.Target that production connectivity doesn't need
// (real targets are expected to resolve via the user's own ~/.ssh/config,
// matching the design spec's "ssh wyzer"-style convention).
type RemoteOption func(*RemoteExecutor)

func WithPort(port int) RemoteOption {
	return func(e *RemoteExecutor) { e.port = port }
}

func WithIdentityFile(path string) RemoteOption {
	return func(e *RemoteExecutor) { e.identityFile = path }
}

func WithConnectTimeout(timeout string) RemoteOption {
	return func(e *RemoteExecutor) { e.connectTimeout = timeout }
}

// WithExtraSSHArgs appends raw arguments to every ssh invocation,
// inserted before the destination — an escape hatch tests use for
// StrictHostKeyChecking=no and a scratch UserKnownHostsFile against a
// freshly generated, otherwise-unknown host key.
func WithExtraSSHArgs(args ...string) RemoteOption {
	return func(e *RemoteExecutor) { e.extraArgs = append(e.extraArgs, args...) }
}

// NewRemoteExecutor constructs a RemoteExecutor for the given host/user.
func NewRemoteExecutor(host, user string, opts ...RemoteOption) *RemoteExecutor {
	e := &RemoteExecutor{
		host:           host,
		user:           user,
		connectTimeout: defaultConnectTimeout,
		controlPersist: defaultControlPersist,
	}
	for _, opt := range opts {
		opt(e)
	}
	e.controlPath = controlPathFor(host, user, e.port)
	return e
}

func controlPathFor(host, user string, port int) string {
	dir := filepath.Join(os.TempDir(), "loomux", "ssh-cm")
	name := fmt.Sprintf("cm-%s-%s-%d.sock", sanitizeForFilename(user), sanitizeForFilename(host), port)
	return filepath.Join(dir, name)
}

func sanitizeForFilename(s string) string {
	return strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(s)
}

func (e *RemoteExecutor) destination() string {
	return e.user + "@" + e.host
}

// baseArgs are the flags shared by every ssh invocation: never prompt
// interactively and fail fast on an unreachable target (design spec's
// error-handling principle — "never a silent hang"), plus the
// multiplexing this ticket requires.
func (e *RemoteExecutor) baseArgs() []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=" + e.connectTimeout,
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=" + e.controlPersist,
		"-o", "ControlPath=" + e.controlPath,
	}
	if e.port != 0 {
		args = append(args, "-p", strconv.Itoa(e.port))
	}
	if e.identityFile != "" {
		args = append(args, "-i", e.identityFile)
	}
	args = append(args, e.extraArgs...)
	return args
}

// sshExec runs a raw (already shell-quoted) remote command string over
// SSH with this executor's standard flags (BatchMode/ConnectTimeout/
// ControlMaster/etc). err is non-nil only for a genuine SSH-level
// failure — exit 255 (ssh's own signal for a connection-level failure)
// maps to ErrUnreachable, or ssh itself failing to start. Otherwise
// callers interpret exitCode themselves: run() treats non-zero as a
// tmux-command failure, FileExists treats test's own 0/1 convention as
// true/false, RemoveFile treats non-zero as an unexpected rm -f
// failure. Shared by run() and the file-existence/cleanup methods so
// none of them duplicate this SSH-invocation boilerplate.
func (e *RemoteExecutor) sshExec(ctx context.Context, remoteCmd string) (stdout, stderr string, exitCode int, err error) {
	if mkErr := os.MkdirAll(filepath.Dir(e.controlPath), 0o700); mkErr != nil {
		return "", "", -1, fmt.Errorf("targets: create control path dir: %w", mkErr)
	}

	sshArgs := append(e.baseArgs(), e.destination(), remoteCmd)

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout, stderr = outBuf.String(), errBuf.String()

	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		if exitErr.ExitCode() == 255 {
			return stdout, stderr, 255, fmt.Errorf("%w: %s", ErrUnreachable, firstNonEmpty(stderr, runErr.Error()))
		}
		return stdout, stderr, exitErr.ExitCode(), nil
	}
	return "", "", -1, fmt.Errorf("targets: ssh: %w", runErr)
}

// run executes a tmux command remotely: args is shell-quoted per-argument
// and joined into the single command string ssh hands to the remote
// shell (ssh concatenates trailing arguments itself, so without quoting,
// shell metacharacters in e.g. send-keys content would be interpreted
// remotely).
func (e *RemoteExecutor) run(ctx context.Context, args ...string) (string, error) {
	remoteCmd := shellQuoteJoin(append([]string{"tmux"}, args...))
	stdout, stderr, exitCode, err := e.sshExec(ctx, remoteCmd)
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		return "", fmt.Errorf("targets: remote tmux %s: %s", args[0], firstNonEmpty(stderr, fmt.Sprintf("exit status %d", exitCode)))
	}
	return stdout, nil
}

// FileExists reports whether path exists on the target's filesystem, via
// `test -e` (exit 0 = exists, non-zero = doesn't — a normal negative
// result, not a Go-level error, same convention as HasSession).
func (e *RemoteExecutor) FileExists(ctx context.Context, path string) (bool, error) {
	remoteCmd := shellQuoteJoin([]string{"test", "-e", path})
	_, _, exitCode, err := e.sshExec(ctx, remoteCmd)
	if err != nil {
		return false, err
	}
	return exitCode == 0, nil
}

// RemoveFile best-effort removes path from the target's filesystem via
// `rm -f`, which itself doesn't error on a missing file.
func (e *RemoteExecutor) RemoveFile(ctx context.Context, path string) error {
	remoteCmd := shellQuoteJoin([]string{"rm", "-f", path})
	_, stderr, exitCode, err := e.sshExec(ctx, remoteCmd)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("targets: remove file: %s", firstNonEmpty(stderr, fmt.Sprintf("exit status %d", exitCode)))
	}
	return nil
}

func (e *RemoteExecutor) NewSession(ctx context.Context, session, dir, command string) error {
	args := []string{"new-session", "-d", "-s", session}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	if command != "" {
		args = append(args, command)
	}
	_, err := e.run(ctx, args...)
	return err
}

func (e *RemoteExecutor) HasSession(ctx context.Context, session string) (bool, error) {
	_, err := e.run(ctx, "has-session", "-t", session)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrUnreachable) {
		return false, err
	}
	// A non-unreachable failure here means tmux itself ran and reported
	// "no such session" — a normal negative result, not a Go-level error.
	return false, nil
}

func (e *RemoteExecutor) SendKeys(ctx context.Context, target, keys string, enter bool) error {
	if _, err := e.run(ctx, "send-keys", "-t", target, "-l", "--", keys); err != nil {
		return err
	}
	if enter {
		if _, err := e.run(ctx, "send-keys", "-t", target, "Enter"); err != nil {
			return err
		}
	}
	return nil
}

func (e *RemoteExecutor) CapturePane(ctx context.Context, target string) (string, error) {
	return e.run(ctx, "capture-pane", "-t", target, "-p")
}

func (e *RemoteExecutor) KillSession(ctx context.Context, session string) error {
	_, err := e.run(ctx, "kill-session", "-t", session)
	return err
}

// ControlMasterAlive reports whether an SSH ControlMaster is currently
// running for this executor's ControlPath — proof that multiplexing is
// actually in effect rather than each call paying a fresh handshake.
func (e *RemoteExecutor) ControlMasterAlive(ctx context.Context) (bool, error) {
	args := []string{
		"-O", "check",
		"-o", "ControlPath=" + e.controlPath,
	}
	if e.port != 0 {
		args = append(args, "-p", strconv.Itoa(e.port))
	}
	args = append(args, e.extraArgs...)
	args = append(args, e.destination())

	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return false, nil
	}
	return false, fmt.Errorf("targets: ssh -O check: %w", err)
}

// Close proactively tears down the SSH ControlMaster rather than relying
// solely on ControlPersist's idle timeout.
func (e *RemoteExecutor) Close() error {
	args := []string{
		"-O", "exit",
		"-o", "ControlPath=" + e.controlPath,
	}
	if e.port != 0 {
		args = append(args, "-p", strconv.Itoa(e.port))
	}
	args = append(args, e.extraArgs...)
	args = append(args, e.destination())

	cmd := exec.Command("ssh", args...)
	// "no such master" is the expected case when Close is called without
	// any prior operation having established one — not a real error.
	_ = cmd.Run()
	return nil
}

// shellQuoteJoin POSIX-single-quotes each argument and joins them with
// spaces, producing a single string safe to hand to a remote shell —
// required because ssh concatenates trailing arguments itself before the
// remote shell interprets them.
func shellQuoteJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
