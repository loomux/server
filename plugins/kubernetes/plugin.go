// Package kubernetes is the Kubernetes target-provider plugin
// (LOOM-179, docs/design/target-providers.md §8): one pod per machine
// in one namespace, a Secret carrying its sshd files and a
// PersistentVolumeClaim for its data, reached through a headless
// Service. Its executable is cmd/loomux-plugin-kubernetes, in this
// module so that client-go never enters the server's dependency graph.
package kubernetes

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// ManifestJSON is the plugin's plugin.json, the one its executable
// serves at describe.
//
//go:embed plugin.json
var ManifestJSON []byte

// Version is the plugin's, as plugin.json says.
const Version = "0.1.0"

// Problem codes the plugin reports from plugin.check.
const (
	// ProblemClusterUnreachable (error): the API server didn't answer.
	ProblemClusterUnreachable = "cluster_unreachable"
	// ProblemForbidden (error): the credential can't list pods in the
	// namespace: the Role or RoleBinding is missing.
	ProblemForbidden = "forbidden"
	// ProblemServiceMissing (warning): no headless Service of the
	// configured subdomain, so machine names won't resolve.
	ProblemServiceMissing = "service_missing"
	// ProblemNetworkPolicyNotEnforced (warning; error when required): a
	// confined canary pod reached the API server or the internet.
	ProblemNetworkPolicyNotEnforced = "network_policy_not_enforced"
	// ProblemNetworkCheckPending (warning): the canary is still running
	// (the image may be pulling); the next check has the verdict.
	ProblemNetworkCheckPending = "network_policy_check_pending"
	// ProblemNetworkCheckFailed (warning; error when required): the
	// canary couldn't run or didn't finish.
	ProblemNetworkCheckFailed = "network_policy_check_failed"
)

// Plugin is the Kubernetes plugin. Its exported fields are seams for
// tests; New sets them for real use.
type Plugin struct {
	NewClient ClientFactory
	Logger    *log.Logger
	Now       func() time.Time
	// Rand names a canary pod uniquely.
	Rand func() string
	// Poll is how often a wait re-reads a pod; 2 s.
	Poll time.Duration
	// DeleteWait bounds a wait for a pod to be gone before it is made
	// again (start after stop, recreate); 20 s, under the host's 30 s
	// call deadline.
	DeleteWait time.Duration
	// CanaryWait bounds how long a check waits for the canary's verdict
	// before answering "pending"; 15 s.
	CanaryWait time.Duration
	// CanaryTimeout bounds the canary itself, the image pull included;
	// 3 min.
	CanaryTimeout time.Duration

	mu         sync.Mutex
	cfg        Config
	client     kubernetes.Interface
	apiHost    string
	host       protocol.HostInfo
	configured bool
	netcheck   *netcheck
}

// New returns the plugin for real use.
func New() *Plugin {
	return &Plugin{
		NewClient:     defaultClient,
		Logger:        log.New(os.Stderr, "", 0),
		Now:           time.Now,
		Rand:          randomSuffix,
		Poll:          2 * time.Second,
		DeleteWait:    20 * time.Second,
		CanaryWait:    15 * time.Second,
		CanaryTimeout: 3 * time.Minute,
	}
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (p *Plugin) Describe(ctx context.Context) (*plugins.Manifest, error) {
	return plugins.ParseManifest(ManifestJSON)
}

// Configure parses the configuration and makes the client. A change
// forgets the last network-isolation verdict: the host checks again.
func (p *Plugin) Configure(ctx context.Context, cp protocol.ConfigureParams) error {
	cfg, err := parseConfig(cp.Config)
	if err != nil {
		return err
	}
	client, apiHost, err := p.NewClient(cfg)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg, p.client, p.apiHost, p.host, p.configured = cfg, client, apiHost, cp.Host, true
	p.netcheck = nil
	return nil
}

// state is the configuration and client, or unavailable before
// configure.
func (p *Plugin) state() (Config, kubernetes.Interface, protocol.HostInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.configured {
		return Config{}, nil, protocol.HostInfo{}, &rpc.Error{Code: rpc.CodeUnavailable, Message: "not configured yet"}
	}
	return p.cfg, p.client, p.host, nil
}

// Check verifies the credential and the namespace (can it list pods
// there), looks for the headless Service, and tests whether
// NetworkPolicy is enforced with a canary pod (design §8).
func (p *Plugin) Check(ctx context.Context) (protocol.CheckResult, error) {
	cfg, client, _, err := p.state()
	if err != nil {
		return protocol.CheckResult{}, err
	}
	res := protocol.CheckResult{OK: true, Problems: []protocol.Problem{}}
	if _, err := client.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector, Limit: 1}); err != nil {
		code := ProblemClusterUnreachable
		if kerrors.IsForbidden(err) || kerrors.IsUnauthorized(err) {
			code = ProblemForbidden
		}
		res.OK = false
		res.Problems = append(res.Problems, protocol.Problem{
			Code: code, Severity: protocol.SeverityError,
			Message: "can't list pods in namespace " + cfg.Namespace + ": " + safeStatus(err),
		})
		return res, nil
	}
	if _, err := client.CoreV1().Services(cfg.Namespace).Get(ctx, cfg.Subdomain, metav1.GetOptions{}); kerrors.IsNotFound(err) {
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemServiceMissing, Severity: protocol.SeverityWarning,
			Message: fmt.Sprintf("no headless Service %q in namespace %s: machine names won't resolve", cfg.Subdomain, cfg.Namespace),
		})
	}
	state, detail := p.enforcement(ctx, cfg, client, p.CanaryWait)
	severity := protocol.SeverityWarning
	if cfg.RequireNetworkPolicy {
		severity = protocol.SeverityError
	}
	switch state {
	case enforcementEnforced:
	case enforcementNotEnforced:
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemNetworkPolicyNotEnforced, Severity: severity,
			Message: "NetworkPolicy isn't enforced in this cluster (" + detail + "): machines can't be confined; install a policy engine (kube-network-policies, Calico, kube-router) for egress: none",
		})
	case enforcementPending:
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemNetworkCheckPending, Severity: protocol.SeverityWarning,
			Message: "the network-isolation check is still running (the agent image may be pulling); check again for its verdict",
		})
	default:
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemNetworkCheckFailed, Severity: severity,
			Message: "the network-isolation check didn't run: " + detail,
		})
	}
	if severity == protocol.SeverityError && state != enforcementEnforced {
		res.OK = false
	}
	return res, nil
}

func (p *Plugin) Shutdown(ctx context.Context) error { return nil }

// safeStatus is an API error's text for a message: the server's
// status message, truncated. A credential never appears in one.
func safeStatus(err error) string {
	var se *kerrors.StatusError
	if errors.As(err, &se) {
		return truncate(se.ErrStatus.Message, 300)
	}
	return truncate(err.Error(), 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func notFound(id string) error {
	return &rpc.Error{Code: rpc.CodeNotFound, Message: "no machine " + id}
}

func unavailable(err error) error {
	return &rpc.Error{Code: rpc.CodeUnavailable, Message: "kubernetes: " + safeStatus(err)}
}

func invalidParams(format string, args ...any) error {
	return &rpc.Error{Code: rpc.CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func ptr[T any](v T) *T { return &v }
