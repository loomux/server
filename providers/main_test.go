package providers_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loomux-providers-fake-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeBin = filepath.Join(dir, plugins.ExecutableName("fake"))
	build := exec.Command("go", "build", "-o", fakeBin, "github.com/Loomux/server/cmd/loomux-plugin-fake")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building the fake plugin:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// harness is a real plugin manager over the fake, and a providers
// manager over that.
type harness struct {
	store    registry.Store
	pm       *plugins.Manager
	m        *providers.Manager
	pluginID string

	mu     sync.Mutex
	probes []string
	active bool
}

func newHarness(t *testing.T, config map[string]any) *harness {
	t.Helper()
	bundle := t.TempDir()
	dir := filepath.Join(bundle, "fake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), fake.ManifestJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fakeBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugins.ExecutableName("fake")), data, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "p.db"), sqlite.WithMasterKey([]byte("01234567890123456789012345678901")))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{store: store}
	h.pm = plugins.NewManager(store, &plugins.Catalog{BundleDir: bundle}, protocol.HostInfo{Version: "test", InstanceID: "inst"}, nil, nil)
	h.pm.InstanceOptions = func(o *plugins.InstanceOptions) {
		o.RestartBackoff = 10 * time.Millisecond
		o.StopTimeout = 500 * time.Millisecond
	}
	h.m = providers.NewManager(store, h.pm, "inst", nil, nil)
	h.m.PollInterval = 20 * time.Millisecond
	h.m.CreateTimeout = 10 * time.Second
	h.m.Probe = func(ctx context.Context, targetID string) error {
		h.mu.Lock()
		h.probes = append(h.probes, targetID)
		h.mu.Unlock()
		return nil
	}
	h.m.ActiveTask = func(ctx context.Context, targetID string) (bool, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.active, nil
	}
	h.pm.MachineCounter = h.m.MachineCount
	h.pm.UninstallMachines = h.m.UninstallMachines
	h.pm.OnNotify = h.m.HandleNotify
	if config == nil {
		config = map[string]any{}
	}
	v, err := h.pm.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "fake", Config: config})
	if err != nil {
		t.Fatalf("install the fake: %v", err)
	}
	h.pluginID = v.ID
	t.Cleanup(func() {
		h.m.Close()
		_ = h.pm.Close(context.Background())
		_ = store.Close()
	})
	return h
}

func (h *harness) create(t *testing.T, name string, persistent bool) (*registry.Target, *registry.Environment) {
	t.Helper()
	p := persistent
	target, env, err := h.m.Create(context.Background(), providers.CreateRequest{
		Target: registry.Target{Name: name}, PluginID: h.pluginID, Size: "small", Persistent: &p,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return target, env
}

// waitStatus polls the environment behind targetID until it has status.
func (h *harness) waitStatus(t *testing.T, targetID string, status registry.EnvironmentStatus) *registry.Environment {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		env, err := h.m.Get(context.Background(), targetID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if env.Status == status {
			return env
		}
		if time.Now().After(deadline) {
			t.Fatalf("environment is %s (%s), never became %s", env.Status, env.StatusReason, status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) probed(targetID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.probes {
		if p == targetID {
			return true
		}
	}
	return false
}

func (h *harness) setActive(v bool) {
	h.mu.Lock()
	h.active = v
	h.mu.Unlock()
}

// pluginHas asks the fake whether it still has the machine.
func (h *harness) pluginHas(t *testing.T, envID string) bool {
	t.Helper()
	var pe protocol.Environment
	err := h.pm.Call(context.Background(), h.pluginID, protocol.MethodTargetsGet, protocol.IDParams{ID: envID}, &pe)
	return err == nil
}
