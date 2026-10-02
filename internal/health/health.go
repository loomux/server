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

// Shallow returns a cheap, unauthenticated liveness result. It reports
// degraded if the database ping fails, but never performs expensive target
// probes. This is the right probe for Kubernetes liveness/readiness.
func (c *Checker) Shallow(ctx context.Context) Result {
	res := Result{Components: map[string]ComponentResult{}}
	status := StatusHealthy

	if db, ok := c.store.(Pinger); ok {
		if err := db.Ping(ctx); err != nil {
			status = StatusDegraded
			res.Components["database"] = ComponentResult{Status: StatusUnhealthy, Error: err.Error()}
		} else {
			res.Components["database"] = ComponentResult{Status: StatusHealthy}
		}
	} else {
		res.Components["database"] = ComponentResult{Status: StatusDegraded, Error: "store does not support ping"}
	}

	if c.routerCfg.Primary.BaseURL == "" || c.routerCfg.Primary.Model == "" {
		status = StatusDegraded
		res.Components["router_model"] = ComponentResult{Status: StatusUnhealthy, Error: "router model not configured"}
	} else {
		res.Components["router_model"] = ComponentResult{Status: StatusHealthy}
	}

	res.Status = status
	return res
}

// Deep returns an authenticated, component-level health report including
// per-target reachability and the sidecar SOCKS5 port. Expensive probes
// use a short timeout so the endpoint stays probe-friendly.
func (c *Checker) Deep(ctx context.Context) Result {
	res := c.Shallow(ctx)

	targetsRes := ComponentResult{Status: StatusHealthy, Detail: []targetDetail{}}
	targets, err := c.store.ListTargets(ctx)
	if err != nil {
		targetsRes.Status = StatusUnhealthy
		targetsRes.Error = fmt.Sprintf("list targets: %v", err)
		targetsRes.Detail = nil
	} else {
		details := make([]targetDetail, 0, len(targets))
		for _, t := range targets {
			d := targetDetail{ID: t.ID, Name: t.Name, Kind: string(t.Kind)}
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			exec, perr := c.newExecutor(t)
			if perr == nil {
				_, perr = exec.RunOnce(probeCtx, "tmux -V")
			}
			cancel()
			if perr != nil {
				targetsRes.Status = StatusDegraded
				d.Status = StatusUnhealthy
				d.Error = perr.Error()
			} else {
				d.Status = StatusHealthy
			}
			details = append(details, d)
		}
		targetsRes.Detail = details
	}
	res.Components["targets"] = targetsRes
	if targetsRes.Status != StatusHealthy && res.Status == StatusHealthy {
		res.Status = targetsRes.Status
	}

	sidecar := c.checkSidecar(ctx)
	res.Components["sidecar_socks5"] = sidecar
	if sidecar.Status != StatusHealthy && res.Status == StatusHealthy {
		// A dead sidecar breaks remote targets but local dispatch still
		// works, so the overall status is degraded rather than fully
		// unhealthy.
		res.Status = StatusDegraded
	}

	return res
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
