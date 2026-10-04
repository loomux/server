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
	"sync"
	"time"
)

const (
	defaultConnectTimeout = "10s"
	defaultControlPersist = "10m"
	// A half-dead connection (the Tailscale sidecar restarted, a target
	// went away) is noticed after ServerAliveInterval*ServerAliveCountMax
	// = 45s of silence instead of hanging (LOOM-84).
	serverAliveInterval = "15"
	serverAliveCountMax = "3"
	// defaultOpTimeout bounds every ssh operation, and defaultRunOnceTimeout
	// a one-shot command (a version probe, a pre-trust script), which may
	// take longer (LOOM-84).
	defaultOpTimeout      = 30 * time.Second
	defaultRunOnceTimeout = 2 * time.Minute
	// maxConcurrentOps caps the ssh operations in flight per target: they
	// share one ControlMaster, and sshd refuses sessions past its
	// MaxSessions (10 by default) on one connection (LOOM-84).
	maxConcurrentOps = 6
)

// opSlots holds, per ControlMaster path (one per target), the semaphore
// bounding concurrent ssh operations on it. Shared by every
// RemoteExecutor for that target, whichever code made it.
var opSlots sync.Map // controlPath -> chan struct{}

func slotsFor(controlPath string) chan struct{} {
	v, _ := opSlots.LoadOrStore(controlPath, make(chan struct{}, maxConcurrentOps))
	return v.(chan struct{})
}

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
	opTimeout      time.Duration
	runOnceTimeout time.Duration
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

// WithOpTimeout overrides the per-operation deadline (LOOM-84) for
// every operation but RunOnce.
func WithOpTimeout(d time.Duration) RemoteOption {
	return func(e *RemoteExecutor) { e.opTimeout = d }
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
		opTimeout:      defaultOpTimeout,
		runOnceTimeout: defaultRunOnceTimeout,
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
		// ssh's own warnings (a new known host, sc1's post-quantum key
		// exchange notice) would otherwise land in RunOnce's output.
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=" + e.connectTimeout,
		"-o", "ServerAliveInterval=" + serverAliveInterval,
		"-o", "ServerAliveCountMax=" + serverAliveCountMax,
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

// sshExec runs a raw (already POSIX-shell-quoted) remote command string over
// SSH with this executor's standard flags (BatchMode/ConnectTimeout/
// ControlMaster/etc). err is non-nil only for a genuine SSH-level
// failure — exit 255 (ssh's own signal for a connection-level failure)
// maps to ErrUnreachable, or ssh itself failing to start. Otherwise
// callers interpret exitCode themselves: run() treats non-zero as a
// tmux-command failure, FileExists treats test's own 0/1 convention as
// true/false, RemoveFile treats non-zero as an unexpected rm -f
// failure. Shared by run() and the file-existence/cleanup methods so
// none of them duplicate this SSH-invocation boilerplate.
//
// Each call holds one of the target's operation slots (LOOM-84) and is
// bounded by timeout: an operation that outlives it fails as
// ErrUnreachable, since a target that doesn't answer in time is, for
// the caller, unreachable.
func (e *RemoteExecutor) sshExec(ctx context.Context, remoteCmd string) (stdout, stderr string, exitCode int, err error) {
	return e.sshExecWithin(ctx, e.opTimeout, remoteCmd)
}

func (e *RemoteExecutor) sshExecWithin(ctx context.Context, timeout time.Duration, remoteCmd string) (stdout, stderr string, exitCode int, err error) {
	if mkErr := os.MkdirAll(filepath.Dir(e.controlPath), 0o700); mkErr != nil {
		return "", "", -1, fmt.Errorf("targets: create control path dir: %w", mkErr)
	}
	slots := slotsFor(e.controlPath)
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return "", "", -1, ctx.Err()
	}
	opCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		opCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// sshd hands the remote command to the user's login shell, which may
	// not be POSIX (fish), while remoteCmd is POSIX sh. So the only thing
	// that shell parses is "/bin/sh"; the real command goes to that sh as
	// a script on stdin (LOOM-90 review). The braces make sh read the
	// whole command before running any of it, and give it /dev/null as
	// stdin so nothing it runs can swallow the script.
	sshArgs := append(e.baseArgs(), e.destination(), "/bin/sh")

	cmd := exec.CommandContext(opCtx, "ssh", sshArgs...)
	cmd.Stdin = strings.NewReader("{\n" + remoteCmd + "\n} </dev/null\n")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout, stderr = outBuf.String(), errBuf.String()

	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	if ctx.Err() == nil && errors.Is(opCtx.Err(), context.DeadlineExceeded) {
		return stdout, stderr, -1, fmt.Errorf("%w: no answer within %s", ErrUnreachable, timeout)
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
	_, err := e.run(ctx, newSessionArgs(session, dir, command)...)
	return err
}

func (e *RemoteExecutor) PaneExited(ctx context.Context, target string) (*PaneExit, error) {
	return paneExited(ctx, e.run, target)
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

// SendKey sends one key by its tmux name to target.
func (e *RemoteExecutor) SendKey(ctx context.Context, target, key string) error {
	if _, err := e.run(ctx, "send-keys", "-t", target, key); err != nil {
		return fmt.Errorf("targets: send key: %w", err)
	}
	return nil
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

// RunOnce runs command once, non-interactively, over SSH (via sshExec —
// the same connection-multiplexed primitive run() and the file-existence
// methods share) and returns its combined stdout+stderr — e.g. many CLIs
// print --version to stderr, so both streams are captured rather than
// just stdout.
func (e *RemoteExecutor) RunOnce(ctx context.Context, command string) (string, error) {
	stdout, stderr, exitCode, err := e.sshExecWithin(ctx, e.runOnceTimeout, command)
	if err != nil {
		return "", err
	}
	output := stdout + stderr
	if exitCode != 0 {
		return output, fmt.Errorf("targets: run once: %s: exit status %d", command, exitCode)
	}
	return output, nil
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
