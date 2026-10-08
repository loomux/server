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
	"net"
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
}

// NewChecker constructs a Checker. sidecarAddr is the SOCKS5 proxy address
// to probe (e.g. "127.0.0.1:1055"); empty disables the sidecar check.
func NewChecker(store registry.Store, newExecutor orchestrator.ExecutorFactory, routerCfg llmrouter.Config, sidecarAddr string) *Checker {
	return &Checker{
		store:       store,
		newExecutor: newExecutor,
		routerCfg:   routerCfg,
		sidecarAddr: sidecarAddr,
	}
}

// deepTimeout bounds all of Deep's probes together (LOOM-162): they run
// at once, under one deadline, so the endpoint answers within it however
// many targets there are.
var deepTimeout = 10 * time.Second

// unavailable is the error Shallow gives a failed component: it answers
// anonymous callers, so the detail (a database error, what isn't
// configured) stays in Deep, which needs a session (LOOM-162).
const unavailable = "unavailable"

// Shallow returns a cheap, unauthenticated liveness result. It reports
// unhealthy if the database ping fails (nothing works without it) and
// degraded if the router model isn't configured, but never performs
// expensive target probes. This is the right probe for Kubernetes
// liveness/readiness. A failed component's error is the fixed
// "unavailable"; Deep has the detail.
func (c *Checker) Shallow(ctx context.Context) Result {
	res := c.core(ctx)
	for name, comp := range res.Components {
		if comp.Error != "" {
			comp.Error = unavailable
			res.Components[name] = comp
		}
	}
	return res
}

// core checks the database and the router configuration, with the
// failures' detail.
func (c *Checker) core(ctx context.Context) Result {
	res := Result{Components: map[string]ComponentResult{}}
	status := StatusHealthy

	if c.routerCfg.Primary.BaseURL == "" || c.routerCfg.Primary.Model == "" {
		status = StatusDegraded
		res.Components["router_model"] = ComponentResult{Status: StatusUnhealthy, Error: "router model not configured"}
	} else {
		res.Components["router_model"] = ComponentResult{Status: StatusHealthy}
	}

	if db, ok := c.store.(Pinger); ok {
		if err := db.Ping(ctx); err != nil {
			status = StatusUnhealthy
			res.Components["database"] = ComponentResult{Status: StatusUnhealthy, Error: err.Error()}
		} else {
			res.Components["database"] = ComponentResult{Status: StatusHealthy}
		}
	} else {
		if status == StatusHealthy {
			status = StatusDegraded
		}
		res.Components["database"] = ComponentResult{Status: StatusDegraded, Error: "store does not support ping"}
	}

	res.Status = status
	return res
}

// Deep returns an authenticated, component-level health report including
// per-target reachability and the sidecar SOCKS5 port, with each
// failure's detail. The target probes and the sidecar dial run at once,
// all under deepTimeout: a probe still running then is reported as timed
// out rather than waited for.
func (c *Checker) Deep(ctx context.Context) Result {
	ctx, cancel := context.WithTimeout(ctx, deepTimeout)
	defer cancel()
	res := c.core(ctx)

	sidecarCh := make(chan ComponentResult, 1)
	go func() { sidecarCh <- c.checkSidecar(ctx) }()

	targetsRes := ComponentResult{Status: StatusHealthy, Detail: []targetDetail{}}
	targets, err := c.store.ListTargets(ctx)
	if err != nil {
		targetsRes.Status = StatusUnhealthy
		targetsRes.Error = fmt.Sprintf("list targets: %v", err)
		targetsRes.Detail = nil
	} else {
		details := c.probeTargets(ctx, targets)
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

	var sidecar ComponentResult
	select {
	case sidecar = <-sidecarCh:
	case <-ctx.Done():
		sidecar = ComponentResult{Status: StatusUnhealthy, Error: "timed out"}
	}
	res.Components["sidecar_socks5"] = sidecar
	if sidecar.Status != StatusHealthy && res.Status == StatusHealthy {
		// A dead sidecar breaks remote targets but local dispatch still
		// works, so the overall status is degraded rather than fully
		// unhealthy.
		res.Status = StatusDegraded
	}

	return res
}

// probeTargets runs `tmux -V` on every target at once and returns their
// details in list's order. One that hasn't answered when ctx ends is
// reported timed out; its probe finishes on its own, unread.
func (c *Checker) probeTargets(ctx context.Context, list []*registry.Target) []targetDetail {
	type probed struct {
		i   int
		err error
	}
	results := make(chan probed, len(list))
	for i, t := range list {
		go func() {
			exec, err := c.newExecutor(t)
			if err == nil {
				_, err = exec.RunOnce(ctx, "tmux -V")
			}
			results <- probed{i, err}
		}()
	}

	details := make([]targetDetail, len(list))
	answered := make([]bool, len(list))
collect:
	for range list {
		select {
		case p := <-results:
			answered[p.i] = true
			details[p.i].Status = StatusHealthy
			if p.err != nil {
				details[p.i].Status = StatusUnhealthy
				details[p.i].Error = p.err.Error()
			}
		case <-ctx.Done():
			break collect
		}
	}
	for i, t := range list {
		details[i].ID, details[i].Name, details[i].Kind = t.ID, t.Name, string(t.Kind)
		if !answered[i] {
			details[i].Status = StatusUnhealthy
			details[i].Error = "timed out"
		}
	}
	return details
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
