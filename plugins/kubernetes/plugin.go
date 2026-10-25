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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sync"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/plugins/sdk"
)

// ManifestJSON is the plugin's plugin.json, the one its executable
// serves at describe.
//
//go:embed plugin.json
var ManifestJSON []byte

// Version is the plugin's, as plugin.json says: 0.0.0-dev in the tree,
// the release a build was stamped with (plugins/manifeststamp).
var Version = manifestVersion()

func manifestVersion() string {
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(ManifestJSON, &m); err != nil || m.Version == "" {
		return "unknown"
	}
	return m.Version
}

// Problem codes the plugin reports from plugin.check.
const (
	// ProblemClusterUnreachable (error): the API server didn't answer.
	ProblemClusterUnreachable = "cluster_unreachable"
	// ProblemForbidden (error): the credential can't list pods in the
	// namespace: the Role or RoleBinding is missing.
	ProblemForbidden = "forbidden"
	// ProblemServiceMissing (warning): no headless Service of the
	// configured subdomain, so machine names won't resolve; or the
	// credential can't read Services, or the read failed, to tell.
	ProblemServiceMissing = "service_missing"
	// ProblemNetworkPolicyNotEnforced (warning; error when required): a
	// confined canary pod reached the API server, kube-dns beyond port
	// 53, the metadata address or the internet.
	ProblemNetworkPolicyNotEnforced = "network_policy_not_enforced"
	// ProblemNetworkPolicyAllowsEgress (warning; error when required):
	// the namespace's NetworkPolicies let an egress "none" machine reach
	// more than kube-dns, or none isolates it for egress (LOOM-201).
	ProblemNetworkPolicyAllowsEgress = "network_policy_allows_egress"
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
	// RenewEvery is how often the nodes of egress "none" machines are
	// looked at for a verdict to renew (nodecheck.go); 1 min.
	RenewEvery time.Duration

	// createMu serializes a new machine's quota count and record, so two
	// creates at once can't both take the last place (LOOM-201).
	createMu sync.Mutex

	mu     sync.Mutex
	cfg    Config
	client kubernetes.Interface
	host   protocol.HostInfo
	// gen counts the configurations; p.cfg.gen is the current one.
	gen        uint64
	configured bool
	// netcheck is the latest canary started, settled the latest one
	// finished: describe acts on settled while a new one runs.
	netcheck *netcheck
	settled  *netcheck
	// nodes is each node's canaries, and watch each egress "none"
	// machine's pod (by name) and its node, whose verdict the renewer
	// keeps current (LOOM-208, nodecheck.go); stopRenew ends this
	// configuration's renewer.
	nodes     map[string]*nodeCheck
	watch     map[string]string
	stopRenew chan struct{}
	// seq numbers the canaries as they start.
	seq uint64
	// inflight is the canary pods this process is still reading: a
	// sweep leaves them to their readers.
	inflight map[string]bool
	// undeleted is each canary whose verdict is recorded but whose
	// delete failed, for the sweep to try again (LOOM-212).
	undeleted map[string]undeletedCanary
	// policy is what the NetworkPolicies were last found to be, for the
	// "none" machines running (LOOM-211, nodecheck.go).
	policy policyRead
	// notifier tells the host when a machine's status changed on its
	// own (LOOM-213): a node's verdict that moves its "none" machines.
	notifier sdk.Notifier
	// verdictHook, if set, runs once check has its verdict, before it
	// reports the nodes known open (tests: a node canary finishing then).
	verdictHook func()
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
		RenewEvery:    time.Minute,
		nodes:         map[string]*nodeCheck{},
		watch:         map[string]string{},
		inflight:      map[string]bool{},
		undeleted:     map[string]undeletedCanary{},
	}
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Notifier makes the plugin a notifying one (sdk.Notifying, LOOM-213).
func (p *Plugin) Notifier() *sdk.Notifier { return &p.notifier }

func (p *Plugin) Describe(ctx context.Context) (*plugins.Manifest, error) {
	return plugins.ParseManifest(ManifestJSON)
}

// Configure parses the configuration and makes the client. A change
// forgets the network-isolation verdicts, the nodes' too: the host
// checks again.
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
	if p.configured && !slices.Equal(p.cfg.TrustedMachines, cfg.TrustedMachines) {
		go p.trustChanged(p.cfg, cfg, client, cp.Host.InstanceID)
	}
	p.gen++
	cfg.gen, cfg.apiHost = p.gen, apiHost
	p.cfg, p.client, p.host, p.configured = cfg, client, cp.Host, true
	p.netcheck, p.settled = nil, nil
	p.nodes, p.watch, p.policy = map[string]*nodeCheck{}, map[string]string{}, policyRead{}
	if p.stopRenew != nil {
		close(p.stopRenew)
	}
	p.stopRenew = make(chan struct{})
	go p.renewNodes(p.stopRenew, p.RenewEvery)
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
	cfg, client, host, err := p.state()
	if err != nil {
		return protocol.CheckResult{}, err
	}
	res := protocol.CheckResult{OK: true, Problems: []protocol.Problem{}}
	severity := protocol.SeverityWarning
	if cfg.RequireNetworkPolicy {
		severity = protocol.SeverityError
	}
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
		res.Problems = append(res.Problems, p.openNodes(severity)...)
		return res, nil
	}
	switch _, err := client.CoreV1().Services(cfg.Namespace).Get(ctx, cfg.Subdomain, metav1.GetOptions{}); {
	case kerrors.IsNotFound(err):
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemServiceMissing, Severity: protocol.SeverityWarning,
			Message: fmt.Sprintf("no headless Service %q in namespace %s: machine names won't resolve", cfg.Subdomain, cfg.Namespace),
		})
	case kerrors.IsForbidden(err):
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemServiceMissing, Severity: protocol.SeverityWarning,
			Message: fmt.Sprintf("can't read Service %q in namespace %s (the Role lacks services get): whether machine names resolve is unknown", cfg.Subdomain, cfg.Namespace),
		})
	case err != nil:
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemServiceMissing, Severity: protocol.SeverityWarning,
			Message: fmt.Sprintf("can't read Service %q in namespace %s: %s: whether machine names resolve is unknown", cfg.Subdomain, cfg.Namespace, safeStatus(err)),
		})
	}
	state, detail := p.enforcement(ctx, cfg, client, host.InstanceID, p.CanaryWait)
	if p.verdictHook != nil {
		p.verdictHook()
	}
	switch state {
	case enforcementEnforced:
	case enforcementNotEnforced:
		res.Problems = append(res.Problems, notEnforced(severity, detail))
	case enforcementPolicyOpen:
		res.Problems = append(res.Problems, protocol.Problem{
			Code: ProblemNetworkPolicyAllowsEgress, Severity: severity,
			Message: "egress: none isn't offered: " + detail + "; a confined machine may reach kube-dns (UDP and TCP 53) and nothing else (design §10 item 6)",
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
	// A node known open, if that isn't the verdict already: a policy
	// error or open policies answered first (LOOM-208).
	if state != enforcementNotEnforced {
		res.Problems = append(res.Problems, p.openNodes(severity)...)
	}
	if pr := trustedPolicyProblem(ctx, client, cfg, host.InstanceID); pr != nil {
		res.Problems = append(res.Problems, *pr)
	}
	// Not OK on any error, a node found open since the verdict too, and
	// on any verdict but "enforced" when the policy is required (pending
	// is only a warning).
	for _, pr := range res.Problems {
		if pr.Severity == protocol.SeverityError {
			res.OK = false
		}
	}
	if severity == protocol.SeverityError && state != enforcementEnforced {
		res.OK = false
	}
	return res, nil
}

// notEnforced is the problem of a canary that got out.
func notEnforced(severity, detail string) protocol.Problem {
	return protocol.Problem{
		Code: ProblemNetworkPolicyNotEnforced, Severity: severity,
		Message: "NetworkPolicy isn't enforced in this cluster (" + detail + "): machines can't be confined; install a policy engine (kube-network-policies, Calico, kube-router) for egress: none",
	}
}

// openNodes is the problem of the nodes known open, if any: check
// reports them whatever else it can't do (LOOM-208).
func (p *Plugin) openNodes(severity string) []protocol.Problem {
	if state, detail := p.nodeFailure(); state == enforcementNotEnforced {
		return []protocol.Problem{notEnforced(severity, detail)}
	}
	return nil
}

func (p *Plugin) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopRenew != nil {
		close(p.stopRenew)
		p.stopRenew = nil
	}
	return nil
}

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
