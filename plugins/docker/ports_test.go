package docker

import (
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

func TestParsePortRange(t *testing.T) {
	c, err := parseConfig(baseConfig(t, map[string]any{"port_range": "32000-32099"}))
	if err != nil || c.PortRange != (portRange{Lo: 32000, Hi: 32099}) {
		t.Fatalf("port_range = %+v, %v", c.PortRange, err)
	}
	if c, err := parseConfig(baseConfig(t, nil)); err != nil || c.PortRange.Lo != 0 {
		t.Errorf("no port_range = %+v, %v", c.PortRange, err)
	}
	// One port per machine at most max_environments: exactly enough is fine.
	if _, err := parseConfig(baseConfig(t, map[string]any{"port_range": "40000-40001", "max_environments": float64(2)})); err != nil {
		t.Errorf("a range of exactly max_environments: %v", err)
	}
	for _, bad := range []string{"32000", "32000-", "-32000", "32000-31999", "1000-2000", "32000-65536", "99999-99999", "3200a-32099", "32000-32099-32100", "+3200-32099"} {
		m := baseConfig(t, nil)
		m["port_range"] = bad
		_, err := parseConfig(m)
		wantCode(t, err, rpc.CodeInvalidConfig)
		if err != nil && !strings.Contains(err.Error(), "port_range must be LO-HI") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	_, err = parseConfig(baseConfig(t, map[string]any{"port_range": "40000-40002"}))
	wantCode(t, err, rpc.CodeInvalidConfig)
	if err == nil || !strings.Contains(err.Error(), "holds 3 ports, fewer than max_environments (5)") {
		t.Errorf("a range smaller than max_environments: %v", err)
	}
}

// rangePlugin is fakePlugin with a port_range and its log kept (set
// before Configure, which starts the plugin's goroutines).
func rangePlugin(t *testing.T, extra map[string]any) (*Plugin, *fakeEngine, *syncLog) {
	t.Helper()
	f := newFakeEngine()
	path := serveUnix(t, f.handler())
	p, logs := newTestPlugin(), &syncLog{}
	p.Logger = log.New(logs, "", 0)
	cfg := map[string]any{"engine": "unix://" + path, "bind_address": "127.0.0.1", "ssh_proxy": "none", "port_range": "40000-40009"}
	for k, v := range extra {
		cfg[k] = v
	}
	if err := p.Configure(ctx, configureParams(t, cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { p.Shutdown(ctx) })
	return p, f, logs
}

type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(b)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Machines take the range's lowest free ports, a stopped machine keeps
// its own, and the docker host's ephemeral range is never used.
func TestPortRangeLowestFree(t *testing.T) {
	p, f, _ := rangePlugin(t, nil)
	for i, id := range []string{"one", "two"} {
		env, err := p.CreateTarget(ctx, spec(id))
		if err != nil || env.Status != protocol.EnvRunning || env.Address.Port != 40000+i {
			t.Fatalf("create %s = %+v, %v", id, env, err)
		}
		if rec := f.volume("lx-" + id + "-ssh"); rec.labels[labelPort] != strconv.Itoa(env.Address.Port) {
			t.Errorf("%s record port = %s", id, rec.labels[labelPort])
		}
	}
	if err := p.StopTarget(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	env, err := p.CreateTarget(ctx, spec("three"))
	if err != nil || env.Address.Port != 40002 {
		t.Fatalf("create beside a stopped machine = %+v, %v", env, err)
	}
	if err := p.StartTarget(ctx, "one"); err != nil {
		t.Errorf("the stopped machine can't start again: %v", err)
	}
	if env, err := p.GetTarget(ctx, "one"); err != nil || env.Address.Port != 40000 {
		t.Errorf("after start = %+v, %v", env, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nextPort != 32768 {
		t.Errorf("the engine's ephemeral allocator was used (cursor %d)", f.nextPort)
	}
}

// A port in the range something else on the host holds is skipped.
func TestPortRangeSkipsHeldPorts(t *testing.T) {
	p, f, _ := rangePlugin(t, nil)
	f.mu.Lock()
	f.taken[40000], f.taken[40001] = true, true
	f.mu.Unlock()
	env, err := p.CreateTarget(ctx, spec("k7f3q2"))
	if err != nil || env.Address.Port != 40002 {
		t.Fatalf("create = %+v, %v", env, err)
	}
	if f.container("lx-k7f3q2-init") != nil {
		t.Error("a probe helper was left behind")
	}
}

// A port taken between the probe and the start: the record is rebuilt
// with the next port of the range.
func TestPortRangeRetriesTakenPort(t *testing.T) {
	p, f, _ := rangePlugin(t, nil)
	f.takeOnHelperRemove = true
	env, err := p.CreateTarget(ctx, spec("k7f3q2"))
	if err != nil || env.Address.Port != 40001 || env.Status != protocol.EnvRunning {
		t.Fatalf("create after a taken port = %+v, %v", env, err)
	}
	if rec := f.volume("lx-k7f3q2-ssh"); rec.labels[labelPort] != "40001" {
		t.Errorf("record port = %s", rec.labels[labelPort])
	}
}

// A range with no port left says so, naming the range and why.
func TestPortRangeExhausted(t *testing.T) {
	p, f, _ := rangePlugin(t, map[string]any{"port_range": "40000-40004"})
	if _, err := p.CreateTarget(ctx, spec("one")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	for port := 40001; port <= 40004; port++ {
		f.taken[port] = true
	}
	f.mu.Unlock()
	_, err := p.CreateTarget(ctx, spec("two"))
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeUnavailable || !strings.Contains(rpcErr.Message, "no free port in port_range 40000-40004: 1 belong to this instance's machines and 4 tried are held by something else") {
		t.Fatalf("want unavailable naming the range, got %v", err)
	}
	if f.volume("lx-two-ssh") != nil {
		t.Error("a record was made without a port")
	}
}

// Machines made at once are each handed their own port at the first
// try: choosing a port and recording it are one step, so none of them
// finds its port taken at start and rebuilds.
func TestPortRangeConcurrentCreates(t *testing.T) {
	p, _, logs := rangePlugin(t, nil)
	ids := []string{"a1", "b2", "c3", "d4", "e5"}
	ports := make([]int, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env, err := p.CreateTarget(ctx, spec(id))
			if err != nil {
				t.Errorf("create %s: %v", id, err)
			}
			ports[i] = env.Address.Port
		}()
	}
	wg.Wait()
	seen := map[int]string{}
	for i, port := range ports {
		if port < 40000 || port > 40009 {
			t.Errorf("%s got port %d, outside the range", ids[i], port)
		}
		if other, dup := seen[port]; dup {
			t.Errorf("%s and %s share port %d", other, ids[i], port)
		}
		seen[port] = ids[i]
	}
	if strings.Contains(logs.String(), "choosing another") {
		t.Errorf("a making rebuilt after its port was taken:\n%s", logs)
	}
}
