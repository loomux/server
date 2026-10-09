package plugins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/registry"
)

// Instance states (InstanceStatus.State).
const (
	// StateRunning: connected, configured, taking calls.
	StateRunning = "running"
	// StateRestarting: the process exited (or the socket dropped) and
	// the instance is bringing it back; calls are unavailable.
	StateRestarting = "restarting"
	// StateFailed: given up (three exits in five minutes, or a handshake
	// that keeps failing); Reason says why. Stop and Start again to
	// retry.
	StateFailed = "failed"
	// StateStopped: Stop was called.
	StateStopped = "stopped"
)

// InstanceStatus is where a running plugin is.
type InstanceStatus struct {
	State  string
	Reason string
}

// Defaults an InstanceOptions zero value gets.
const (
	DefaultCallTimeout       = 30 * time.Second
	DefaultStopTimeout       = 10 * time.Second
	DefaultRestartBackoff    = time.Second
	DefaultReconnectInterval = 2 * time.Second
	// maxFailures within failureWindow fails the instance.
	maxFailures   = 3
	failureWindow = 5 * time.Minute
)

// ErrManifestMismatch is a plugin whose describe doesn't match the
// manifest the host read for it.
var ErrManifestMismatch = errors.New("plugins: the plugin's manifest doesn't match the one on disk")

// ErrUnavailable is a call while the instance isn't running.
var ErrUnavailable = &rpc.Error{Code: rpc.CodeUnavailable, Message: "the plugin isn't running"}

// InstanceOptions is what Start needs.
type InstanceOptions struct {
	Label  string
	Source registry.PluginSource
	// Path is the plugin's directory (subprocess) or socket.
	Path     string
	Manifest *Manifest
	// Config is the whole configuration, secrets included, for
	// plugin.configure.
	Config map[string]any
	Host   protocol.HostInfo
	Logger *slog.Logger
	// OnCall, if set, is told about every call's outcome (for metrics).
	OnCall func(method string, err error)

	CallTimeout       time.Duration
	StopTimeout       time.Duration
	RestartBackoff    time.Duration
	ReconnectInterval time.Duration
}

func (o *InstanceOptions) defaults() {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.CallTimeout == 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.StopTimeout == 0 {
		o.StopTimeout = DefaultStopTimeout
	}
	if o.RestartBackoff == 0 {
		o.RestartBackoff = DefaultRestartBackoff
	}
	if o.ReconnectInterval == 0 {
		o.ReconnectInterval = DefaultReconnectInterval
	}
}

// Instance is one running plugin: a supervised subprocess, or a
// connection to a sidecar's socket, kept up until Stop.
type Instance struct {
	opts InstanceOptions

	mu         sync.Mutex
	conn       *rpc.Conn
	cmd        *exec.Cmd
	exited     chan struct{} // closed when cmd has exited
	state      string
	reason     string
	stopping   bool
	failures   []time.Time
	lastStderr string
	done       chan struct{} // closed when the supervisor has ended
}

// Start starts (or connects to) the plugin, runs the handshake
// (describe must match o.Manifest) and configures it. A handshake
// failure returns the error with nothing left running.
func Start(ctx context.Context, o InstanceOptions) (*Instance, error) {
	o.defaults()
	i := &Instance{opts: o, state: StateRestarting, done: make(chan struct{})}
	if err := i.connect(ctx); err != nil {
		i.teardown()
		close(i.done)
		return nil, err
	}
	i.setState(StateRunning, "")
	go i.supervise()
	return i, nil
}

// Status is the instance's state.
func (i *Instance) Status() InstanceStatus {
	i.mu.Lock()
	defer i.mu.Unlock()
	return InstanceStatus{State: i.state, Reason: i.reason}
}

// Label is the plugin instance's label.
func (i *Instance) Label() string { return i.opts.Label }

// Call sends method to the plugin with the call deadline. While the
// instance isn't running it returns ErrUnavailable.
func (i *Instance) Call(ctx context.Context, method string, params, result any) error {
	i.mu.Lock()
	conn, state := i.conn, i.state
	i.mu.Unlock()
	var err error
	if state != StateRunning || conn == nil {
		err = ErrUnavailable
	} else {
		callCtx, cancel := context.WithTimeout(ctx, i.opts.CallTimeout)
		err = conn.Call(callCtx, method, params, result)
		cancel()
		if errors.Is(err, rpc.ErrClosed) {
			err = ErrUnavailable
		}
	}
	if i.opts.OnCall != nil {
		i.opts.OnCall(method, err)
	}
	return err
}

// Check runs plugin.check.
func (i *Instance) Check(ctx context.Context) (protocol.CheckResult, error) {
	var res protocol.CheckResult
	err := i.Call(ctx, protocol.MethodCheck, nil, &res)
	if res.Problems == nil {
		res.Problems = []protocol.Problem{}
	}
	return res, err
}

// Stop asks the plugin to shut down, then ends the process or the
// connection; a subprocess still alive after StopTimeout is killed.
func (i *Instance) Stop(ctx context.Context) error {
	i.mu.Lock()
	if i.stopping {
		i.mu.Unlock()
		<-i.done
		return nil
	}
	i.stopping = true
	conn := i.conn
	i.mu.Unlock()
	if conn != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, i.opts.StopTimeout)
		_ = conn.Call(shutdownCtx, protocol.MethodShutdown, nil, nil)
		cancel()
	}
	i.teardown()
	<-i.done
	i.setState(StateStopped, "")
	return nil
}

func (i *Instance) setState(state, reason string) {
	i.mu.Lock()
	i.state, i.reason = state, reason
	i.mu.Unlock()
}

// connect spawns or dials, then handshakes and configures.
func (i *Instance) connect(ctx context.Context) error {
	var conn *rpc.Conn
	var err error
	switch i.opts.Source {
	case registry.PluginSourceSocket:
		conn, err = DialSocket(ctx, i.opts.Path)
		if err != nil {
			return fmt.Errorf("plugins: %s: connect: %w", i.opts.Label, err)
		}
	default:
		conn, err = i.spawn()
		if err != nil {
			return err
		}
	}
	i.mu.Lock()
	i.conn = conn
	i.mu.Unlock()
	if err := i.handshake(ctx, conn); err != nil {
		i.teardown()
		return err
	}
	return nil
}

// spawn starts the subprocess with a clean environment and stdio as
// the channel; stderr goes to the log.
func (i *Instance) spawn() (*rpc.Conn, error) {
	exe := filepath.Join(i.opts.Path, ExecutableName(i.opts.Manifest.Name))
	cmd := exec.Command(exe)
	cmd.Dir = i.opts.Path
	cmd.Env = cleanEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugins: %s: %w", i.opts.Label, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugins: %s: %w", i.opts.Label, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("plugins: %s: %w", i.opts.Label, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugins: %s: start %s: %w", i.opts.Label, ExecutableName(i.opts.Manifest.Name), err)
	}
	exited := make(chan struct{})
	go i.readStderr(stderr)
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	i.mu.Lock()
	i.cmd, i.exited = cmd, exited
	i.mu.Unlock()
	return rpc.NewConn(stdout, stdin, nil), nil
}

// cleanEnv is the environment a plugin subprocess gets: what a program
// needs to run, never LOOMUX_*.
func cleanEnv() []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

func (i *Instance) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		i.mu.Lock()
		i.lastStderr = line
		i.mu.Unlock()
		i.opts.Logger.Info("plugin stderr", "plugin", i.opts.Label, "line", line)
	}
}

// handshake runs describe (must match) and configure.
func (i *Instance) handshake(ctx context.Context, conn *rpc.Conn) error {
	hctx, cancel := context.WithTimeout(ctx, i.opts.CallTimeout)
	defer cancel()
	m, err := describe(hctx, conn)
	if err != nil {
		return fmt.Errorf("plugins: %s: %w", i.opts.Label, err)
	}
	if !m.Equal(i.opts.Manifest) {
		return fmt.Errorf("%w: %s says %s %s, the host read %s %s", ErrManifestMismatch, i.opts.Label,
			m.Name, m.Version, i.opts.Manifest.Name, i.opts.Manifest.Version)
	}
	params := protocol.ConfigureParams{Config: i.opts.Config, Host: i.opts.Host}
	if params.Config == nil {
		params.Config = map[string]any{}
	}
	if err := conn.Call(hctx, protocol.MethodConfigure, params, nil); err != nil {
		return fmt.Errorf("plugins: %s: configure: %w", i.opts.Label, err)
	}
	return nil
}

// teardown closes the connection and ends the subprocess.
func (i *Instance) teardown() {
	i.mu.Lock()
	conn, cmd, exited := i.conn, i.cmd, i.exited
	i.conn, i.cmd, i.exited = nil, nil, nil
	i.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	if cmd == nil {
		return
	}
	select {
	case <-exited:
	case <-time.After(i.opts.StopTimeout):
		_ = cmd.Process.Kill()
		<-exited
	}
}

// supervise keeps the plugin up until Stop: a subprocess that exits is
// restarted with backoff, a dropped socket redialled; too many failures
// too close together fail the instance.
func (i *Instance) supervise() {
	defer close(i.done)
	backoff := i.opts.RestartBackoff
	for {
		i.mu.Lock()
		conn := i.conn
		i.mu.Unlock()
		if conn != nil {
			<-conn.Done()
		}
		i.mu.Lock()
		stopping := i.stopping
		reason := i.lastStderr
		i.mu.Unlock()
		if stopping {
			return
		}
		if reason == "" {
			reason = "the plugin stopped answering"
		}
		i.setState(StateRestarting, reason)
		i.teardown()
		if i.tooManyFailures() {
			i.setState(StateFailed, fmt.Sprintf("stopped %d times in %s; last: %s", maxFailures, failureWindow, reason))
			i.opts.Logger.Warn("plugin failed", "plugin", i.opts.Label, "reason", reason)
			return
		}
		i.opts.Logger.Warn("plugin restarting", "plugin", i.opts.Label, "reason", reason)
		wait := backoff
		if i.opts.Source == registry.PluginSourceSocket {
			wait = i.opts.ReconnectInterval
		}
		if !i.sleepUnlessStopping(wait) {
			return
		}
		if backoff < 8*i.opts.RestartBackoff {
			backoff *= 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), i.opts.CallTimeout+i.opts.StopTimeout)
		err := i.connect(ctx)
		cancel()
		// A Stop that arrived while reconnecting has nothing to tear
		// down yet: do it here, or the new process would outlive it.
		i.mu.Lock()
		stopping = i.stopping
		i.mu.Unlock()
		if stopping {
			i.teardown()
			return
		}
		if err != nil {
			i.mu.Lock()
			i.lastStderr = err.Error()
			i.mu.Unlock()
			continue
		}
		i.setState(StateRunning, "")
	}
}

// tooManyFailures records a failure now and reports whether that makes
// maxFailures within failureWindow. A socket plugin's drops don't count:
// it is redialled for as long as the host runs.
func (i *Instance) tooManyFailures() bool {
	if i.opts.Source == registry.PluginSourceSocket {
		return false
	}
	now := time.Now()
	i.mu.Lock()
	defer i.mu.Unlock()
	kept := i.failures[:0]
	for _, t := range i.failures {
		if now.Sub(t) < failureWindow {
			kept = append(kept, t)
		}
	}
	i.failures = append(kept, now)
	return len(i.failures) >= maxFailures
}

func (i *Instance) sleepUnlessStopping(d time.Duration) bool {
	deadline := time.After(d)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			return true
		case <-tick.C:
			i.mu.Lock()
			stopping := i.stopping
			i.mu.Unlock()
			if stopping {
				return false
			}
		}
	}
}
