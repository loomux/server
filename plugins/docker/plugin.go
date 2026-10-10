// Package docker is the Docker target-provider plugin (LOOM-180,
// docs/design/target-providers.md §9): one container per machine on
// one Docker host, reached over SSH (docker system dial-stdio) or the
// engine's local socket, with a named volume for sshd's files (the
// machine's record) and one for a persistent machine's data. Its
// executable is cmd/loomux-plugin-docker; the module is its own so the
// SSH and SOCKS libraries never enter the server's dependency graph.
package docker

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

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

// Plugin is the Docker plugin. Its exported fields are seams for tests;
// New sets them for real use.
type Plugin struct {
	Logger *log.Logger
	Now    func() time.Time
	// CreateWait bounds how long targets.create waits for the machine
	// to be made before answering with its progress; 20 s, under the
	// host's 30 s call deadline.
	CreateWait time.Duration
	// CreateTimeout bounds the creation itself, the image pull
	// included; 10 min.
	CreateTimeout time.Duration
	// OpTTL is how long a finished creation's outcome (its error) is
	// kept for targets.get; 5 min.
	OpTTL time.Duration

	mu         sync.Mutex
	cfg        Config
	host       protocol.HostInfo
	dialer     dialer
	engine     *engine
	configured bool
	ops        map[string]*createOp
	digests    map[string]string
}

// New returns the plugin for real use.
func New() *Plugin {
	return &Plugin{
		Logger:        log.New(os.Stderr, "", 0),
		Now:           time.Now,
		CreateWait:    20 * time.Second,
		CreateTimeout: 10 * time.Minute,
		OpTTL:         5 * time.Minute,
		ops:           map[string]*createOp{},
		digests:       map[string]string{},
	}
}

func (p *Plugin) Describe(ctx context.Context) (*plugins.Manifest, error) {
	return plugins.ParseManifest(ManifestJSON)
}

// Configure parses the configuration and makes the client; nothing is
// connected until a call needs the engine (check is what verifies).
func (p *Plugin) Configure(ctx context.Context, cp protocol.ConfigureParams) error {
	cfg, err := parseConfig(cp.Config)
	if err != nil {
		return err
	}
	d := newDialer(cfg)
	p.cancelOps()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.engine != nil {
		p.engine.Close()
	}
	if p.dialer != nil {
		p.dialer.Close()
	}
	p.cfg, p.host, p.dialer, p.engine, p.configured = cfg, cp.Host, d, newEngine(d.Dial), true
	return nil
}

// state is the configuration and client, or unavailable before
// configure.
func (p *Plugin) state() (Config, *engine, protocol.HostInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.configured {
		return Config{}, nil, protocol.HostInfo{}, &rpc.Error{Code: rpc.CodeUnavailable, Message: "not configured yet"}
	}
	return p.cfg, p.engine, p.host, nil
}

// Shutdown closes the connection to the engine.
func (p *Plugin) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.engine != nil {
		p.engine.Close()
	}
	if p.dialer != nil {
		p.dialer.Close()
	}
	return nil
}

// Problem codes the plugin reports from plugin.check.
const (
	// ProblemEngineUnreachable (error): the engine didn't answer.
	ProblemEngineUnreachable = "engine_unreachable"
	// ProblemUnauthorized (error): the docker host refused the plugin's
	// key, or the engine refused the user.
	ProblemUnauthorized = "unauthorized"
	// ProblemHostKeyUnpinned (error): ssh_host_key is empty; the
	// message carries the scanned key's type, fingerprint and line to
	// trust.
	ProblemHostKeyUnpinned = "host_key_unpinned"
	// ProblemHostKeyMismatch (error): the host presented another key
	// than the pinned one.
	ProblemHostKeyMismatch = "host_key_mismatch"
	// ProblemEngineTooOld (error): the engine's API is older than 1.41.
	ProblemEngineTooOld = "engine_too_old"
	// ProblemEngineNotLinux (error): the engine runs another OS's
	// containers.
	ProblemEngineNotLinux = "engine_not_linux"
	// ProblemSeccompDisabled (warning): the engine applies no seccomp
	// profile, so machines run without the default syscall filter.
	ProblemSeccompDisabled = "seccomp_disabled"
	// ProblemImageMissing (warning): the agent image isn't on the host
	// yet; the first create pulls it.
	ProblemImageMissing = "image_missing"
	// ProblemNetworkMisconfigured (warning): the agents' network exists
	// without container-to-container traffic off.
	ProblemNetworkMisconfigured = "network_misconfigured"
)

// minAPI is the oldest Engine API the plugin accepts.
const minAPI = "1.41"

// Check verifies the engine answers (the host key pinned, the key
// accepted), speaks a new enough API for Linux containers, applies
// seccomp, has the agent image, and has the agents' network as the
// plugin makes it.
func (p *Plugin) Check(ctx context.Context) (protocol.CheckResult, error) {
	cfg, eng, _, err := p.state()
	if err != nil {
		return protocol.CheckResult{}, err
	}
	res := protocol.CheckResult{OK: true, Problems: []protocol.Problem{}}
	fail := func(code, msg string) protocol.CheckResult {
		res.OK = false
		res.Problems = append(res.Problems, protocol.Problem{Code: code, Severity: protocol.SeverityError, Message: msg})
		return res
	}
	warn := func(code, msg string) {
		res.Problems = append(res.Problems, protocol.Problem{Code: code, Severity: protocol.SeverityWarning, Message: msg})
	}
	api, err := eng.ping(ctx)
	if err != nil {
		var ee *engineError
		switch {
		case errors.Is(err, errHostKeyUnpinned):
			pub, scanErr := p.scan(ctx)
			if scanErr != nil {
				return fail(ProblemEngineUnreachable, "can't reach "+cfg.Engine.String()+": "+oneLine(scanErr.Error())), nil
			}
			line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
			return fail(ProblemHostKeyUnpinned, fmt.Sprintf("the docker host's key isn't pinned: %s %s; to trust it, set ssh_host_key to: %s", pub.Type(), ssh.FingerprintSHA256(pub), line)), nil
		case errors.Is(err, errHostKeyMismatch):
			return fail(ProblemHostKeyMismatch, oneLine(err.Error())+"; if the host was reinstalled, clear ssh_host_key and check again"), nil
		case errors.Is(err, errAuth):
			return fail(ProblemUnauthorized, cfg.Engine.String()+" refused the plugin's key: add its public half to "+cfg.Engine.User+"'s authorized_keys there"), nil
		case errors.As(err, &ee) && (ee.Status == 401 || ee.Status == 403):
			return fail(ProblemUnauthorized, "the engine at "+cfg.Engine.String()+" refused the request: "+ee.Message), nil
		}
		return fail(ProblemEngineUnreachable, "can't reach the docker engine at "+cfg.Engine.String()+": "+oneLine(err.Error())), nil
	}
	if !apiAtLeast(api, minAPI) {
		return fail(ProblemEngineTooOld, "the docker engine speaks API "+api+"; "+minAPI+" (Docker 20.10) or newer is needed"), nil
	}
	var info infoResult
	if err := eng.get(ctx, "/info", nil, &info); err != nil {
		return fail(ProblemEngineUnreachable, "can't read the engine's info: "+oneLine(p.mapErr(err).Error())), nil
	}
	if info.OSType != "" && info.OSType != "linux" {
		return fail(ProblemEngineNotLinux, "the engine runs "+info.OSType+" containers; machines need Linux"), nil
	}
	seccomp := false
	for _, o := range info.SecurityOptions {
		if strings.HasPrefix(o, "name=seccomp") {
			seccomp = true
		}
	}
	if !seccomp {
		warn(ProblemSeccompDisabled, "the docker engine applies no seccomp profile: machines run without the default syscall filter")
	}
	if err := eng.get(ctx, "/images/"+url.PathEscape(cfg.AgentImage)+"/json", nil, nil); isStatus(err, 404) {
		warn(ProblemImageMissing, "the agent image "+cfg.AgentImage+" isn't on the docker host yet; the first machine pulls it, which takes a while")
	}
	var n networkInfo
	if err := eng.get(ctx, "/networks/"+networkName, nil, &n); err == nil && n.Options["com.docker.network.bridge.enable_icc"] != "false" {
		warn(ProblemNetworkMisconfigured, "the network "+networkName+" exists without enable_icc=false: machines can reach each other; remove it and the plugin makes it again")
	}
	return res, nil
}

// scan learns the docker host's key (ssh engines only).
func (p *Plugin) scan(ctx context.Context) (ssh.PublicKey, error) {
	p.mu.Lock()
	d, _ := p.dialer.(*sshDialer)
	p.mu.Unlock()
	if d == nil {
		return nil, errors.New("not an ssh engine")
	}
	return d.Scan(ctx)
}

// apiAtLeast compares "major.minor" API versions.
func apiAtLeast(v, min string) bool {
	var vMaj, vMin, mMaj, mMin int
	if _, err := fmt.Sscanf(v, "%d.%d", &vMaj, &vMin); err != nil {
		return false
	}
	fmt.Sscanf(min, "%d.%d", &mMaj, &mMin)
	return vMaj > mMaj || vMaj == mMaj && vMin >= mMin
}
