// Package health implements Loomux's deep health checks (LOOM-105).
//
// The cheap GET /api/v1/health endpoint only reports whether the process
// is alive and its core dependencies are reachable enough to serve traffic.
// The authenticated GET /api/v1/health/deep endpoint returns per-component
// detail: database writability, router configuration, per-target probe
// results, and the Tailscale sidecar SOCKS5 port.
package health

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router/llmrouter"
)

// Status values for health responses.
const (
	StatusHealthy   = "healthy"
	StatusDegraded  = "degraded"
	StatusUnhealthy = "unhealthy"
)

// ComponentResult is the health of one subsystem.
type ComponentResult struct {
	Status string `json:"status"`
	// Error is a plain-language failure reason; omitted when healthy.
	Error string `json:"error,omitempty"`
	// Detail holds component-specific structured state.
	Detail any `json:"detail,omitempty"`
}

// Result is the response body for /api/v1/health/deep.
type Result struct {
	Status     string                     `json:"status"`
	Components map[string]ComponentResult `json:"components"`
}

// Pinger is the store capability health needs. *sqlite.Store satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Checker holds the dependencies needed to probe every health component.
type Checker struct {
	store       registry.Store
	newExecutor orchestrator.ExecutorFactory
	routerCfg   llmrouter.Config
	sidecarAddr string
	deepTimeout time.Duration
	logger      *slog.Logger
}

// DefaultDeepTimeout bounds a whole Deep check: every probe runs at once
// under this one deadline, however many targets there are (LOOM-162).
const DefaultDeepTimeout = 10 * time.Second

// Fixed component errors for the unauthenticated Shallow check
// (LOOM-162): an anonymous caller learns that a component is down, not
// why. The cause is logged and stays in the authenticated Deep result.
const (
	ErrTextUnavailable   = "unavailable"
	ErrTextNotConfigured = "not configured"
)

// Option configures a Checker.
type Option func(*Checker)

// WithDeepTimeout replaces DefaultDeepTimeout.
func WithDeepTimeout(d time.Duration) Option {
	return func(c *Checker) { c.deepTimeout = d }
}

// WithLogger logs why a Shallow check found a component down, since its
// response doesn't say.
func WithLogger(l *slog.Logger) Option {
	return func(c *Checker) { c.logger = l }
}

// NewChecker constructs a Checker. sidecarAddr is the SOCKS5 proxy address
// to probe (e.g. "127.0.0.1:1055"); empty disables the sidecar check.
func NewChecker(store registry.Store, newExecutor orchestrator.ExecutorFactory, routerCfg llmrouter.Config, sidecarAddr string, opts ...Option) *Checker {
	c := &Checker{
		store:       store,
		newExecutor: newExecutor,
		routerCfg:   routerCfg,
		sidecarAddr: sidecarAddr,
		deepTimeout: DefaultDeepTimeout,
		logger:      slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Shallow returns a cheap, unauthenticated liveness result. It reports
// unhealthy if the database ping fails (nothing works without it) and
// degraded if the router model isn't configured, but never performs
// expensive target probes. Component errors are fixed strings: this is
// the right probe for Kubernetes liveness/readiness, and anyone can
// call it.
func (c *Checker) Shallow(ctx context.Context) Result {
	res := c.core(ctx)
	for name, comp := range res.Components {
		if comp.Error == "" {
			continue
		}
		c.logger.Warn("health check component down", "component", name, "status", comp.Status, "error", comp.Error)
		if name == "router_model" {
			comp.Error = ErrTextNotConfigured
		} else {
			comp.Error = ErrTextUnavailable
		}
		res.Components[name] = comp
	}
	return res
}

// core is the Shallow check with each component's real error.
func (c *Checker) core(ctx context.Context) Result {
	res := Result{Components: map[string]ComponentResult{}}
	status := StatusHealthy

	if db, ok := c.store.(Pinger); ok {
		if err := db.Ping(ctx); err != nil {
			status = StatusUnhealthy
			res.Components["database"] = ComponentResult{Status: StatusUnhealthy, Error: err.Error()}
		} else {
			res.Components["database"] = ComponentResult{Status: StatusHealthy}
		}
	} else {
		status = StatusDegraded
		res.Components["database"] = ComponentResult{Status: StatusDegraded, Error: "store does not support ping"}
	}

	if c.routerCfg.Primary.BaseURL == "" || c.routerCfg.Primary.Model == "" {
		if status == StatusHealthy {
			status = StatusDegraded
		}
		res.Components["router_model"] = ComponentResult{Status: StatusUnhealthy, Error: "router model not configured"}
	} else {
		res.Components["router_model"] = ComponentResult{Status: StatusHealthy}
	}

	res.Status = status
	return res
}

// Deep returns an authenticated, component-level health report including
// per-target reachability and the sidecar SOCKS5 port. Every probe runs
// at once under one deadline (the Checker's deep timeout), so the
// endpoint answers in about that long however many targets there are.
func (c *Checker) Deep(ctx context.Context) Result {
	ctx, cancel := context.WithTimeout(ctx, c.deepTimeout)
	defer cancel()

	var sidecar ComponentResult
	sidecarDone := make(chan struct{})
	go func() {
		defer close(sidecarDone)
		sidecar = c.checkSidecar(ctx)
	}()

	res := c.core(ctx)

	targetsRes := ComponentResult{Status: StatusHealthy, Detail: []targetDetail{}}
	targets, err := c.store.ListTargets(ctx)
	if err != nil {
		targetsRes.Status = StatusUnhealthy
		targetsRes.Error = fmt.Sprintf("list targets: %v", err)
		targetsRes.Detail = nil
	} else {
		details := make([]targetDetail, len(targets))
		var wg sync.WaitGroup
		for i, t := range targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				details[i] = c.probeTarget(ctx, t)
			}()
		}
		wg.Wait()
		for _, d := range details {
			if d.Status != StatusHealthy {
				targetsRes.Status = StatusDegraded
			}
		}
		targetsRes.Detail = details
	}
	res.Components["targets"] = targetsRes
	if targetsRes.Status != StatusHealthy && res.Status == StatusHealthy {
		res.Status = targetsRes.Status
	}

	<-sidecarDone
	res.Components["sidecar_socks5"] = sidecar
	if sidecar.Status != StatusHealthy && res.Status == StatusHealthy {
		// A dead sidecar breaks remote targets but local dispatch still
		// works, so the overall status is degraded rather than fully
		// unhealthy.
		res.Status = StatusDegraded
	}

	return res
}

// probeTarget checks that t answers "tmux -V" before ctx's deadline.
func (c *Checker) probeTarget(ctx context.Context, t *registry.Target) targetDetail {
	d := targetDetail{ID: t.ID, Name: t.Name, Kind: string(t.Kind), Status: StatusHealthy}
	exec, err := c.newExecutor(t)
	if err == nil {
		_, err = exec.RunOnce(ctx, "tmux -V")
		_ = exec.Close()
	}
	if err != nil {
		d.Status = StatusUnhealthy
		d.Error = err.Error()
		if ctx.Err() != nil {
			d.Error = fmt.Sprintf("no answer within %s: %v", c.deepTimeout, err)
		}
	}
	return d
}

func (c *Checker) checkSidecar(ctx context.Context) ComponentResult {
	if c.sidecarAddr == "" {
		return ComponentResult{Status: StatusHealthy, Error: "not configured"}
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.sidecarAddr)
	if err != nil {
		return ComponentResult{Status: StatusUnhealthy, Error: fmt.Sprintf("dial %s: %v", c.sidecarAddr, err)}
	}
	_ = conn.Close()
	return ComponentResult{Status: StatusHealthy}
}

type targetDetail struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// DefaultSidecarAddr is the production Tailscale sidecar SOCKS5 address
// as configured in theWyseKube's services/base/loomux/30-loomuxd.yaml.
const DefaultSidecarAddr = "127.0.0.1:1055"
