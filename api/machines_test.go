package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
)

// stubMachines scripts the providers manager; it writes the target rows
// into the store like the real one, so the handlers' reads see them.
type stubMachines struct {
	store    registry.Store
	envs     map[string]*registry.Environment // by target id
	err      error
	lastReq  providers.CreateRequest
	lastSize string
	calls    []string
}

func (s *stubMachines) view(id string) (*providers.MachineView, error) {
	env, ok := s.envs[id]
	if !ok {
		return nil, providers.ErrNotMachine
	}
	return &providers.MachineView{Environment: *env, PluginLabel: "wysekube", UpdateAvailable: env.Image == "old",
		Warnings: []protocol.Problem{{Code: "network_policy_not_enforced", Message: "no engine", Severity: "warning"}}}, nil
}
func (s *stubMachines) View(ctx context.Context, id string) (*providers.MachineView, error) {
	return s.view(id)
}
func (s *stubMachines) Views(ctx context.Context) (map[string]*providers.MachineView, error) {
	out := map[string]*providers.MachineView{}
	for id := range s.envs {
		v, _ := s.view(id)
		out[id] = v
	}
	return out, nil
}
func (s *stubMachines) IsMachine(ctx context.Context, id string) bool { _, ok := s.envs[id]; return ok }
func (s *stubMachines) Create(ctx context.Context, req providers.CreateRequest) (*registry.Target, *registry.Environment, error) {
	s.lastReq = req
	if s.err != nil {
		return nil, nil, s.err
	}
	t := req.Target
	t.ID, t.Kind, t.Host, t.User, t.SSHPort, t.WorkspaceRoot = uuid.NewString(), registry.TargetKindRemote, "lx-abc.fake.invalid", "agent", 2222, "/data/work"
	if err := s.store.CreateTarget(ctx, &t); err != nil {
		return nil, nil, err
	}
	env := &registry.Environment{ID: "abc", TargetID: t.ID, PluginID: req.PluginID, PluginName: "fake", PluginVersion: "0.1.0",
		Status: registry.EnvironmentCreating, Size: req.Size, Persistent: true, Egress: "internet", Image: "ghcr.io/loomux/agent:fake"}
	s.envs[t.ID] = env
	return &t, env, nil
}
func (s *stubMachines) op(name, id string) error {
	s.calls = append(s.calls, name+":"+id)
	if s.err != nil {
		return s.err
	}
	if _, ok := s.envs[id]; !ok {
		return providers.ErrNotMachine
	}
	return nil
}
func (s *stubMachines) Start(ctx context.Context, id string) error { return s.op("start", id) }
func (s *stubMachines) Stop(ctx context.Context, id string) error  { return s.op("stop", id) }
func (s *stubMachines) Recreate(ctx context.Context, id, size string) error {
	s.lastSize = size
	return s.op("recreate", id)
}
func (s *stubMachines) DeleteTarget(ctx context.Context, id string) error {
	if err := s.op("delete", id); err != nil {
		return err
	}
	delete(s.envs, id)
	return s.store.DeleteTarget(ctx, id)
}
func (s *stubMachines) AttachCommands(ctx context.Context, id, session string) ([]protocol.AttachCommand, error) {
	if _, ok := s.envs[id]; !ok {
		return nil, providers.ErrNotMachine
	}
	return []protocol.AttachCommand{{Via: "kubectl", Command: "kubectl exec -it lx-abc -- tmux -L loomux attach -t " + session}}, nil
}

func machineServer(t *testing.T) (base, token string, stub *stubMachines, store registry.Store) {
	t.Helper()
	stub = &stubMachines{envs: map[string]*registry.Environment{}}
	srv, _, st := newTestServerWith(t, func(st registry.Store) []api.Option {
		stub.store = st
		return []api.Option{api.WithMachines(stub)}
	})
	token, _ = login(t, srv.URL, testPassword)
	return srv.URL, token, stub, st
}

func TestMachines_CreateViaTargets(t *testing.T) {
	base, token, stub, _ := machineServer(t)
	status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{
		"name": "builds", "purpose": "work", "plugin": map[string]any{"id": "p1", "size": "medium", "egress": "internet"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create machine: %d %s", status, body)
	}
	var resp struct {
		ID, Kind, Host, User string
		SSHPort              int `json:"ssh_port"`
		Plugin               *struct {
			ID, Label, Status, Size string
			Warnings                []map[string]string
			UpdateAvailable         bool `json:"update_available"`
		}
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Kind != "remote" || resp.Host != "lx-abc.fake.invalid" || resp.User != "agent" || resp.SSHPort != 2222 {
		t.Errorf("target = %+v", resp)
	}
	if resp.Plugin == nil || resp.Plugin.ID != "p1" || resp.Plugin.Label != "wysekube" || resp.Plugin.Status != "creating" || resp.Plugin.Size != "medium" || len(resp.Plugin.Warnings) != 1 {
		t.Errorf("plugin = %+v", resp.Plugin)
	}
	if stub.lastReq.PluginID != "p1" || stub.lastReq.Size != "medium" || stub.lastReq.Target.Name != "builds" || stub.lastReq.Target.Policy.Purpose != "work" {
		t.Errorf("create request = %+v", stub.lastReq)
	}

	// The list and GET /targets/{id} carry the plugin object; a
	// registered host's is null.
	status, body = doJSON(t, "GET", base+"/api/v1/targets/"+resp.ID, token, nil)
	if status != http.StatusOK || !strings.Contains(body, `"plugin":{"id":"p1"`) {
		t.Errorf("get: %d %s", status, body)
	}
	if status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "laptop", "kind": "remote", "host": "laptop.example", "user": "me"}); status != http.StatusCreated || !strings.Contains(body, `"plugin":null`) {
		t.Errorf("registered host: %d %s", status, body)
	}
	status, body = doJSON(t, "GET", base+"/api/v1/targets", token, nil)
	if status != http.StatusOK || !strings.Contains(body, `"plugin":{"id":"p1"`) || !strings.Contains(body, `"plugin":null`) {
		t.Errorf("list: %d %s", status, body)
	}
	if status, _ := doJSON(t, "GET", base+"/api/v1/targets/nope", token, nil); status != http.StatusNotFound {
		t.Errorf("get unknown: %d", status)
	}
}

func TestMachines_CreateRefusesConnectionFields(t *testing.T) {
	base, token, stub, _ := machineServer(t)
	for field, value := range map[string]any{"host": "x", "user": "u", "ssh_port": 22, "ssh_key_id": "k", "generate_ssh_key": true, "ssh_proxy": "none", "workspace_root": "/w"} {
		status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "m", "plugin": map[string]any{"id": "p1"}, field: value})
		if status != http.StatusBadRequest || !strings.Contains(body, field) {
			t.Errorf("%s with a plugin: %d %s", field, status, body)
		}
	}
	if status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "m", "kind": "local", "plugin": map[string]any{"id": "p1"}}); status != http.StatusBadRequest || !strings.Contains(body, "kind") {
		t.Errorf("kind local with a plugin: %d %s", status, body)
	}
	if status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "m", "plugin": map[string]any{}}); status != http.StatusBadRequest || !strings.Contains(body, "plugin.id") {
		t.Errorf("plugin without an id: %d %s", status, body)
	}
	cases := []struct {
		err    error
		status int
		text   string
	}{
		{providers.ErrPluginUnavailable, http.StatusConflict, "plugin"},
		{&providers.InvalidError{Field: "size", Msg: "must be one of small"}, http.StatusBadRequest, "size: must be one of small"},
		{providers.ErrQuota, http.StatusConflict, "limit"},
		{registry.ErrNoMasterKey, http.StatusServiceUnavailable, "LOOMUX_MASTER_KEY"},
		{&providers.BadAddressError{Detail: "host"}, http.StatusBadGateway, "address"},
	}
	for _, c := range cases {
		stub.err = c.err
		status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "m", "plugin": map[string]any{"id": "p1"}})
		if status != c.status || !strings.Contains(body, c.text) {
			t.Errorf("%v: %d %s", c.err, status, body)
		}
	}
}

func TestMachines_LifecycleAndDelete(t *testing.T) {
	base, token, stub, store := machineServer(t)
	status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "m", "plugin": map[string]any{"id": "p1"}})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal([]byte(body), &created)

	for _, verb := range []string{"stop", "start"} {
		if status, body := doJSON(t, "POST", base+"/api/v1/targets/"+created.ID+"/"+verb, token, nil); status != http.StatusAccepted || !strings.Contains(body, `"plugin":{`) {
			t.Errorf("%s: %d %s", verb, status, body)
		}
	}
	if status, _ := doJSON(t, "POST", base+"/api/v1/targets/"+created.ID+"/recreate", token, map[string]any{"size": "large"}); status != http.StatusAccepted || stub.lastSize != "large" {
		t.Errorf("recreate: %d (size %q)", status, stub.lastSize)
	}
	if status, _ := doJSON(t, "POST", base+"/api/v1/targets/nope/start", token, nil); status != http.StatusNotFound {
		t.Errorf("start unknown: %d", status)
	}
	// Routes a machine's connection isn't the user's to change.
	for _, r := range [][2]string{{"POST", "/scan-host-key"}, {"POST", "/pin"}, {"DELETE", "/pin"}, {"POST", "/migrate-ssh"}} {
		status, body := doJSON(t, r[0], base+"/api/v1/targets/"+created.ID+r[1], token, map[string]any{"fingerprint": "x"})
		if status != http.StatusConflict || !strings.Contains(body, "plugin") {
			t.Errorf("%s %s on a machine: %d %s", r[0], r[1], status, body)
		}
	}
	// PUT may change the name and policy, not the connection.
	if status, body := doJSON(t, "PUT", base+"/api/v1/targets/"+created.ID, token, map[string]any{"name": "renamed", "kind": "remote", "host": "lx-abc.fake.invalid", "user": "agent", "ssh_port": 2222, "workspace_root": "/data/work"}); status != http.StatusOK {
		t.Errorf("rename: %d %s", status, body)
	}
	if status, body := doJSON(t, "PUT", base+"/api/v1/targets/"+created.ID, token, map[string]any{"name": "renamed", "kind": "remote", "host": "elsewhere", "user": "agent"}); status != http.StatusBadRequest || !strings.Contains(body, "plugin") {
		t.Errorf("host change: %d %s", status, body)
	}

	// Registered hosts aren't machines.
	plain := &registry.Target{ID: uuid.NewString(), Name: "laptop", Kind: registry.TargetKindRemote, Host: "laptop.example", User: "me"}
	if err := store.CreateTarget(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	if status, body := doJSON(t, "POST", base+"/api/v1/targets/"+plain.ID+"/stop", token, nil); status != http.StatusConflict || !strings.Contains(body, "registered host") {
		t.Errorf("stop a registered host: %d %s", status, body)
	}

	errCases := []struct {
		err    error
		status int
	}{
		{providers.ErrTaskActive, http.StatusConflict},
		{providers.ErrEphemeralStop, http.StatusConflict},
		{providers.ErrDetached, http.StatusConflict},
		{&providers.BadStateError{Op: "stop", Status: registry.EnvironmentCreating}, http.StatusConflict},
		{errors.New("boom"), http.StatusInternalServerError},
	}
	for _, c := range errCases {
		stub.err = c.err
		if status, _ := doJSON(t, "POST", base+"/api/v1/targets/"+created.ID+"/stop", token, nil); status != c.status {
			t.Errorf("stop with %v: %d", c.err, status)
		}
	}
	stub.err = providers.ErrTaskActive
	if status, _ := doJSON(t, "DELETE", base+"/api/v1/targets/"+created.ID, token, nil); status != http.StatusConflict {
		t.Errorf("delete with a task running: %d", status)
	}
	stub.err = nil
	if status, _ := doJSON(t, "DELETE", base+"/api/v1/targets/"+created.ID, token, nil); status != http.StatusNoContent {
		t.Errorf("delete machine: %d", status)
	}
	if _, err := store.GetTarget(context.Background(), created.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Error("the machine's target survived")
	}
	if status, _ := doJSON(t, "DELETE", base+"/api/v1/targets/"+plain.ID, token, nil); status != http.StatusNoContent {
		t.Errorf("delete a registered host still works: %d", status)
	}
}

func TestMachines_NotConfigured(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	status, body := doJSON(t, "POST", srv.URL+"/api/v1/targets", token, map[string]any{"name": "m", "plugin": map[string]any{"id": "p1"}})
	if status != http.StatusNotFound || !strings.Contains(body, "not available") {
		t.Errorf("create machine without machines: %d %s", status, body)
	}
	if status, _ := doJSON(t, "POST", srv.URL+"/api/v1/targets/x/start", token, nil); status != http.StatusNotFound {
		t.Errorf("start without machines: %d", status)
	}
}

func TestCredentials_TargetScope(t *testing.T) {
	base, token, vault := credentialServer(t)
	ctx := context.Background()
	target := &registry.Target{ID: uuid.NewString(), Name: "machine", Kind: registry.TargetKindRemote, Host: "lx.fake.invalid", User: "agent"}
	if err := vault.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	status, body := doJSON(t, "POST", base+"/api/v1/credentials", token, map[string]string{"name": "ANTHROPIC_API_KEY", "value": "sk-SECRET", "target_id": target.ID})
	if status != http.StatusCreated || !strings.Contains(body, `"target_id":"`+target.ID+`"`) || strings.Contains(body, "SECRET") {
		t.Fatalf("create: %d %s", status, body)
	}
	if status, body := doJSON(t, "GET", base+"/api/v1/credentials", token, nil); status != http.StatusOK || !strings.Contains(body, `"target_id":"`+target.ID+`"`) {
		t.Errorf("list: %d %s", status, body)
	}
	if status, body := doJSON(t, "POST", base+"/api/v1/credentials", token, map[string]string{"name": "X", "value": "v", "target_id": "no-such-target"}); status != http.StatusBadRequest || !strings.Contains(body, "target") {
		t.Errorf("unknown target: %d %s", status, body)
	}
}
