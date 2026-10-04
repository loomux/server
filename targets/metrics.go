package targets

import (
	"context"
	"time"

	"github.com/Loomux/server/internal/metrics"
)

// opName returns a stable operation name for metrics. It is intentionally
// a small fixed set rather than the full tmux command.
func opName(args ...string) string {
	if len(args) == 0 {
		return "unknown"
	}
	switch args[0] {
	case "new-session":
		return "new_session"
	case "has-session":
		return "has_session"
	case "send-keys":
		return "send_keys"
	case "capture-pane":
		return "capture_pane"
	case "kill-session":
		return "kill_session"
	default:
		return args[0]
	}
}

// metricsExecutor wraps a TargetExecutor and records Prometheus metrics
// for every operation (LOOM-103).
type metricsExecutor struct {
	inner   TargetExecutor
	metrics *metrics.Metrics
	kind    string
	name    string
}

func newMetricsExecutor(inner TargetExecutor, m *metrics.Metrics, kind, name string) TargetExecutor {
	if m == nil {
		return inner
	}
	return &metricsExecutor{inner: inner, metrics: m, kind: kind, name: name}
}

func (e *metricsExecutor) NewSession(ctx context.Context, session, dir, command string) error {
	start := time.Now()
	err := e.inner.NewSession(ctx, session, dir, command)
	e.metrics.RecordTargetOp(e.kind, "new_session", time.Since(start), "", err)
	return err
}

func (e *metricsExecutor) PaneExited(ctx context.Context, target string) (*PaneExit, error) {
	start := time.Now()
	exit, err := e.inner.PaneExited(ctx, target)
	e.metrics.RecordTargetOp(e.kind, "pane_exited", time.Since(start), "", err)
	return exit, err
}

func (e *metricsExecutor) HasSession(ctx context.Context, session string) (bool, error) {
	start := time.Now()
	ok, err := e.inner.HasSession(ctx, session)
	e.metrics.RecordTargetOp(e.kind, "has_session", time.Since(start), "", err)
	return ok, err
}

func (e *metricsExecutor) SendKeys(ctx context.Context, target, keys string, enter bool) error {
	start := time.Now()
	err := e.inner.SendKeys(ctx, target, keys, enter)
	e.metrics.RecordTargetOp(e.kind, "send_keys", time.Since(start), "", err)
	return err
}

func (e *metricsExecutor) SendKey(ctx context.Context, target, key string) error {
	start := time.Now()
	err := e.inner.SendKey(ctx, target, key)
	e.metrics.RecordTargetOp(e.kind, "send_key", time.Since(start), "", err)
	return err
}

func (e *metricsExecutor) CapturePane(ctx context.Context, target string) (string, error) {
	start := time.Now()
	out, err := e.inner.CapturePane(ctx, target)
	e.metrics.RecordTargetOp(e.kind, "capture_pane", time.Since(start), "", err)
	return out, err
}

func (e *metricsExecutor) KillSession(ctx context.Context, session string) error {
	start := time.Now()
	err := e.inner.KillSession(ctx, session)
	e.metrics.RecordTargetOp(e.kind, "kill_session", time.Since(start), "", err)
	return err
}

func (e *metricsExecutor) Close() error {
	return e.inner.Close()
}

func (e *metricsExecutor) FileExists(ctx context.Context, path string) (bool, error) {
	start := time.Now()
	ok, err := e.inner.FileExists(ctx, path)
	e.metrics.RecordTargetOp(e.kind, "file_exists", time.Since(start), "", err)
	return ok, err
}

func (e *metricsExecutor) RemoveFile(ctx context.Context, path string) error {
	start := time.Now()
	err := e.inner.RemoveFile(ctx, path)
	e.metrics.RecordTargetOp(e.kind, "remove_file", time.Since(start), "", err)
	return err
}

func (e *metricsExecutor) RunOnce(ctx context.Context, command string) (string, error) {
	start := time.Now()
	out, err := e.inner.RunOnce(ctx, command)
	e.metrics.RecordTargetOp(e.kind, "run_once", time.Since(start), "", err)
	return out, err
}
