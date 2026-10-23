package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// LOOM-216: what the plugin refuses reaches the chat as a reason; what
// fails in Loomux stays an error.
func TestMachineRefusal(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&providers.InvalidError{Field: "egress", Msg: "must be one of internet, none"}, "egress must be one of internet, none."},
		{fmt.Errorf("%w (5)", providers.ErrQuota), "the plugin's machine limit is reached; delete a machine it made first."},
		{providers.ErrPluginUnavailable, "that plugin isn't installed, enabled and running."},
		{&plugins.CapabilityMissingError{Method: "targets.create", Capability: "targets.create"}, "that plugin doesn't make machines."},
		{&providers.BadAddressError{Detail: "no port"}, "the plugin's offer can't be used (no port)."},
		{fmt.Errorf("create: %w", registry.ErrConflict), "a target with that name exists."},
	} {
		var refusal *router.MachineRefusal
		if err := machineRefusal(tc.err); !errors.As(err, &refusal) || refusal.Reason != tc.want {
			t.Errorf("%v → %v, want refusal %q", tc.err, err, tc.want)
		}
	}
	boom := errors.New("database is locked")
	if err := machineRefusal(boom); err != boom {
		t.Errorf("an internal error became %v", err)
	}
}

// A chat's machine is a target with only its name, and asks the plugin
// about trusted for that name.
func TestMachineRequest(t *testing.T) {
	yes := true
	req := request(router.MachineSpec{PluginID: "p", Name: "cc", Size: "medium", Persistent: &yes, Egress: "trusted"})
	if !req.NamedTrust || req.Target.Name != "cc" || req.PluginID != "p" || req.Size != "medium" || *req.Persistent != true || req.Egress != "trusted" {
		t.Errorf("request = %+v", req)
	}
	if !reflect.DeepEqual(req.Target.Policy, registry.TargetPolicy{}) {
		t.Errorf("policy = %+v, want a created machine's default", req.Target.Policy)
	}
}

// LOOM-216 end to end: the real routing model's provision_target, the
// router's offer and a yes, through providers to the fake plugin. The
// prompt lists the plugin; trusted is offered and made only for a name
// its trusted_machines lists.
func TestBuild_MachineFromChat(t *testing.T) {
	var mu sync.Mutex
	var args string
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		prompts = append(prompts, string(body))
		a := args
		mu.Unlock()
		resp := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "route_decision", "arguments": a}}},
			}}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	cfg := testConfig(t, srv.URL)
	cfg.MasterKey = []byte("01234567890123456789012345678901")
	cfg.PluginBundleDir = buildFakePlugin(t)
	ctx := context.Background()
	app, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer app.Close()
	v, err := app.Plugins().Install(ctx, plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "lab",
		Config: map[string]any{"trusted_machines": "cc"}})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	propose := func(name string) {
		mu.Lock()
		args = `{"action":"provision_target","new_target":{"plugin_id":"` + v.ID + `","name":"` + name + `","size":"medium","egress":"trusted"}}`
		mu.Unlock()
	}

	propose("builds")
	reply, err := app.Dispatch(ctx, "conv-1", "a machine called builds on the LAN", "")
	if err != nil || !strings.Contains(reply, "I can't create that machine: egress trusted is only for the machines the plugin's trusted_machines names") {
		t.Fatalf("reply = %q, %v", reply, err)
	}
	mu.Lock()
	last := prompts[len(prompts)-1]
	mu.Unlock()
	if !strings.Contains(last, "plugin_id: "+v.ID) || !strings.Contains(last, "fake (lab)") || !strings.Contains(last, "provision_target") {
		t.Errorf("the routing prompt doesn't offer the plugin:\n%s", last)
	}

	propose("cc")
	reply, err = app.Dispatch(ctx, "conv-1", "the hub machine, cc, on the LAN", "")
	if err != nil || !strings.Contains(reply, "trusted: the LAN and the tailnet") || !strings.Contains(reply, "3Gi memory") {
		t.Fatalf("reply = %q, %v", reply, err)
	}
	if list, _ := app.Store().ListTargets(ctx); len(list) != 0 {
		t.Fatalf("a target exists before the yes: %+v", list)
	}
	reply, err = app.Dispatch(ctx, "conv-1", "yes", "")
	if err != nil || !strings.Contains(reply, "Creating cc with the fake (lab) plugin") {
		t.Fatalf("reply = %q, %v", reply, err)
	}
	list, err := app.Store().ListTargets(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "cc" {
		t.Fatalf("targets = %+v, %v", list, err)
	}
	env, err := app.Machines().Get(ctx, list[0].ID)
	if err != nil || env.Egress != "trusted" || env.Size != "medium" || !env.Persistent || env.PluginID != v.ID {
		t.Errorf("machine = %+v, %v", env, err)
	}
}
