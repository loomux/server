package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/targets"
)

type droppedKeys struct {
	mu  sync.Mutex
	ids []string
}

func (d *droppedKeys) drop(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = append(d.ids, id)
}

// managedServer is a test server on one store with a master key, serving
// targets and SSH keys from it (as production does).
func managedServer(t *testing.T, masterKey []byte, opts ...api.Option) (base, token string, store registry.Store, dropped *droppedKeys) {
	t.Helper()
	var sopts []sqlite.Option
	if masterKey != nil {
		sopts = append(sopts, sqlite.WithMasterKey(masterKey))
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "managed.db"), sopts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dropped = &droppedKeys{}
	srv, _, _ := newTestServerOn(t, st, func(s registry.Store) []api.Option {
		return append([]api.Option{api.WithSSHKeys(s), api.WithSSHKeyDropper(dropped.drop)}, opts...)
	})
	token, _ = login(t, srv.URL, testPassword)
	return srv.URL, token, st, dropped
}

type managedTargetJSON struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	SSHMode  string `json:"ssh_mode"`
	SSHProxy string `json:"ssh_proxy"`
	SSHKey   *struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Fingerprint string `json:"fingerprint"`
		PublicKey   string `json:"public_key"`
	} `json:"ssh_key"`
	Ready    bool    `json:"ready"`
	NextStep *string `json:"next_step"`
}

func decodeTarget(t *testing.T, body string) managedTargetJSON {
	t.Helper()
	var out managedTargetJSON
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func nextStep(m managedTargetJSON) string {
	if m.NextStep == nil {
		return "<null>"
	}
	return *m.NextStep
}

var testMasterKey = bytes.Repeat([]byte("k"), 32)

// LOOM-138: a target registered with generate_ssh_key gets a key of its
// own, shows its public key, and says what to do next.
func TestManagedTarget_CreateWithGeneratedKey(t *testing.T) {
	base, token, store, _ := managedServer(t, testMasterKey)
	status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{
		"name": "wyzer box", "kind": "remote", "host": "wyzer.tail1234.ts.net", "user": "orski", "generate_ssh_key": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	got := decodeTarget(t, body)
	if got.SSHMode != "managed" || got.SSHProxy != "default" || got.SSHKey == nil || got.Ready || nextStep(got) != "pin_host_key" {
		t.Fatalf("created = %+v (next_step %s)", got, nextStep(got))
	}
	if !strings.HasPrefix(got.SSHKey.PublicKey, "ssh-ed25519 ") || !strings.HasSuffix(got.SSHKey.PublicKey, " loomux-"+got.SSHKey.Name) ||
		!targets.ValidSSHKeyName(got.SSHKey.Name) {
		t.Errorf("ssh_key = %+v", got.SSHKey)
	}
	stored, err := store.GetTarget(context.Background(), got.ID)
	if err != nil || stored.SSHKeyRef != got.SSHKey.ID {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	k, err := store.GetSSHKey(context.Background(), got.SSHKey.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertNoPrivateKey(t, "create", body, k)
	_, list := doJSON(t, "GET", base+"/api/v1/targets", token, nil)
	assertNoPrivateKey(t, "list", list, k)
	if !strings.Contains(list, got.SSHKey.Fingerprint) || !strings.Contains(list, `"ssh_mode":"managed"`) {
		t.Errorf("list lacks the managed target's key: %s", list)
	}
}

func TestManagedTarget_CreateWithExistingKey(t *testing.T) {
	base, token, _, _ := managedServer(t, testMasterKey)
	_, body := doJSON(t, "POST", base+"/api/v1/ssh-keys", token, map[string]string{"name": "fleet"})
	var key sshKeyJSON
	_ = json.Unmarshal([]byte(body), &key)
	status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{
		"name": "a", "kind": "remote", "host": "10.0.0.7", "user": "orski", "ssh_key_id": key.ID, "ssh_proxy": "none",
	})
	got := decodeTarget(t, body)
	if status != http.StatusCreated || got.SSHKey == nil || got.SSHKey.ID != key.ID || got.SSHProxy != "none" {
		t.Fatalf("create: %d %s", status, body)
	}
	if status, body := doJSON(t, "DELETE", base+"/api/v1/ssh-keys/"+key.ID, token, nil); status != http.StatusConflict {
		t.Errorf("delete a key in use: %d %s", status, body)
	}
	// Back to the SSH config: ssh_proxy goes back to the config's with it.
	status, body = doJSON(t, "PUT", base+"/api/v1/targets/"+got.ID, token, map[string]any{
		"name": "a", "kind": "remote", "host": "10.0.0.7", "user": "orski", "ssh_key_id": "",
	})
	if back := decodeTarget(t, body); status != http.StatusOK || back.SSHMode != "config" || back.SSHProxy != "default" {
		t.Errorf("back to config mode: %d %s", status, body)
	}
}

func TestManagedTarget_RefusesBadInput(t *testing.T) {
	base, token, store, _ := managedServer(t, testMasterKey)
	_, body := doJSON(t, "POST", base+"/api/v1/ssh-keys", token, map[string]string{"name": "fleet"})
	var key sshKeyJSON
	_ = json.Unmarshal([]byte(body), &key)
	for name, tc := range map[string]struct {
		req  map[string]any
		want string
	}{
		"unknown key":           {map[string]any{"ssh_key_id": "nope"}, "no such SSH key"},
		"key and generate":      {map[string]any{"ssh_key_id": key.ID, "generate_ssh_key": true}, "not both"},
		"shell in host":         {map[string]any{"ssh_key_id": key.ID, "host": "a;id"}, "host must be"},
		"proxy token in host":   {map[string]any{"ssh_key_id": key.ID, "host": "%h"}, "host must be"},
		"alias-only host":       {map[string]any{"ssh_key_id": key.ID, "host": "a_b"}, "host must be"},
		"option as host":        {map[string]any{"generate_ssh_key": true, "host": "-oProxyCommand=id"}, "host must be"},
		"unknown proxy":         {map[string]any{"ssh_key_id": key.ID, "ssh_proxy": "socks5://evil:1"}, "ssh_proxy"},
		"proxy without a key":   {map[string]any{"ssh_proxy": "none"}, "ssh_proxy"},
		"key on a local target": {map[string]any{"kind": "local", "host": "", "user": "", "ssh_key_id": key.ID}, "remote"},
		"generate for a local":  {map[string]any{"kind": "local", "host": "", "user": "", "generate_ssh_key": true}, "remote"},
	} {
		req := map[string]any{"name": "x-" + strings.ReplaceAll(name, " ", "-"), "kind": "remote", "host": "box.example", "user": "u"}
		for k, v := range tc.req {
			req[k] = v
		}
		status, body := doJSON(t, "POST", base+"/api/v1/targets", token, req)
		if status != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s, want 400 mentioning %q", name, status, body, tc.want)
		}
	}
	// A refused target leaves no generated key behind.
	if keys, _ := store.ListSSHKeys(context.Background()); len(keys) != 1 {
		t.Errorf("%d keys after refused creates, want just the one made on purpose", len(keys))
	}
}

// A duplicate target name is still reported as such.
func TestManagedTarget_DuplicateNameKeepsNoKey(t *testing.T) {
	base, token, store, _ := managedServer(t, testMasterKey)
	req := map[string]any{"name": "dup", "kind": "remote", "host": "box.example", "user": "u", "generate_ssh_key": true}
	if status, body := doJSON(t, "POST", base+"/api/v1/targets", token, req); status != http.StatusCreated {
		t.Fatalf("first: %d %s", status, body)
	}
	status, body := doJSON(t, "POST", base+"/api/v1/targets", token, req)
	if status != http.StatusConflict || !strings.Contains(body, "name already exists") {
		t.Errorf("duplicate: %d %s", status, body)
	}
	if keys, _ := store.ListSSHKeys(context.Background()); len(keys) != 1 {
		t.Errorf("%d keys, want the first target's only", len(keys))
	}
}

func TestManagedTarget_GenerateWithoutMasterKey(t *testing.T) {
	base, token, store, _ := managedServer(t, nil)
	status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{
		"name": "a", "kind": "remote", "host": "box.example", "user": "u", "generate_ssh_key": true,
	})
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "LOOMUX_MASTER_KEY") {
		t.Errorf("generate without a master key: %d %s", status, body)
	}
	if ts, _ := store.ListTargets(context.Background()); len(ts) != 0 {
		t.Errorf("target created anyway: %+v", ts)
	}
}

// A managed target whose address changes must have its new host key
// confirmed: the pin is dropped. Turning it back into a config-mode
// target keeps the pin (the same machine, reached another way).
func TestManagedTarget_AddressChangeDropsPin(t *testing.T) {
	base, token, store, _ := managedServer(t, testMasterKey)
	ctx := context.Background()
	_, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{
		"name": "a", "kind": "remote", "host": "box.example", "user": "u", "generate_ssh_key": true,
	})
	created := decodeTarget(t, body)
	pin := func() {
		t.Helper()
		if err := store.SetTargetHostKeys(ctx, created.ID, testHostKeyLine); err != nil {
			t.Fatal(err)
		}
	}
	put := func(req map[string]any) managedTargetJSON {
		t.Helper()
		full := map[string]any{"name": "a", "kind": "remote", "host": "box.example", "user": "u"}
		for k, v := range req {
			full[k] = v
		}
		status, body := doJSON(t, "PUT", base+"/api/v1/targets/"+created.ID, token, full)
		if status != http.StatusOK {
			t.Fatalf("PUT %v: %d %s", req, status, body)
		}
		return decodeTarget(t, body)
	}
	pinned := func() bool {
		got, _ := store.GetTarget(ctx, created.ID)
		return got.HostKeys != ""
	}

	pin()
	if got := put(map[string]any{"user": "u"}); !pinned() || nextStep(got) == "pin_host_key" {
		t.Errorf("an unrelated change dropped the pin (next_step %s)", nextStep(got))
	}
	if got := put(map[string]any{"host": "other.example"}); pinned() || nextStep(got) != "pin_host_key" {
		t.Errorf("host change kept the pin (next_step %s)", nextStep(got))
	}
	pin()
	if put(map[string]any{"host": "other.example", "ssh_port": 2222}); pinned() {
		t.Error("port change kept the pin")
	}
	pin()
	if got := put(map[string]any{"host": "other.example", "ssh_port": 2222, "ssh_key_id": ""}); !pinned() || got.SSHMode != "config" || got.SSHKey != nil {
		t.Errorf("back to config mode = %+v, pinned %v", got, pinned())
	}
}

func TestConfigTarget_Fields(t *testing.T) {
	base, token, _, _ := managedServer(t, testMasterKey)
	status, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "a", "kind": "remote", "host": "wyzer", "user": "u"})
	got := decodeTarget(t, body)
	if status != http.StatusCreated || got.SSHMode != "config" || got.SSHKey != nil || got.SSHProxy != "default" || !strings.Contains(body, `"ssh_key":null`) {
		t.Errorf("config-mode target = %d %s", status, body)
	}
}

func TestSSHKeys_DeleteStopsTheAgent(t *testing.T) {
	base, token, _, dropped := managedServer(t, testMasterKey)
	_, body := doJSON(t, "POST", base+"/api/v1/ssh-keys", token, map[string]string{"name": "k"})
	var key sshKeyJSON
	_ = json.Unmarshal([]byte(body), &key)
	if status, _ := doJSON(t, "DELETE", base+"/api/v1/ssh-keys/"+key.ID, token, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	if len(dropped.ids) != 1 || dropped.ids[0] != key.ID {
		t.Errorf("dropped = %v, want [%s]", dropped.ids, key.ID)
	}
}

// LOOM-138: test says which step failed: connecting, the host key,
// authentication, or tmux.
func TestTestTarget_Steps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health registry.TargetHealth
		want   map[string]string
	}{
		{"all good", registry.TargetHealth{Reachable: true, TmuxVersion: "tmux 3.5"},
			map[string]string{"connect": "ok", "host_key": "ok", "auth": "ok", "tmux": "ok"}},
		{"auth", registry.TargetHealth{Error: (&targets.UnreachableError{Host: "b", Failure: targets.SSHAuthFailed, Managed: true}).Error()},
			map[string]string{"connect": "ok", "host_key": "ok", "auth": "failed", "tmux": "skipped"}},
		{"host key", registry.TargetHealth{Error: (&targets.UnreachableError{Host: "b", Failure: targets.SSHHostKeyUnknown}).Error()},
			map[string]string{"connect": "ok", "host_key": "failed", "auth": "skipped", "tmux": "skipped"}},
		{"proxy", registry.TargetHealth{Error: (&targets.UnreachableError{Host: "b", Failure: targets.SSHProxyUnreachable}).Error()},
			map[string]string{"connect": "failed", "host_key": "skipped", "auth": "skipped", "tmux": "skipped"}},
		{"no tmux", registry.TargetHealth{Reachable: true, Error: "tmux not found"},
			map[string]string{"connect": "ok", "host_key": "ok", "auth": "ok", "tmux": "failed"}},
	} {
		h := tc.health
		h.TargetID, h.ProbedAt = "t1", time.Now()
		base, token, _, _ := managedServer(t, testMasterKey, api.WithTargetProber(&fakeTargetProber{health: &h}))
		status, body := doJSON(t, "POST", base+"/api/v1/targets/t1/test", token, nil)
		var out struct {
			Steps []struct{ Name, Status, Error string } `json:"steps"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		got := map[string]string{}
		for _, s := range out.Steps {
			got[s.Name] = s.Status
			if s.Status == "failed" && s.Error == "" {
				t.Errorf("%s: failed step %s carries no error", tc.name, s.Name)
			}
		}
		if status != http.StatusOK || len(out.Steps) != 4 || toJSON(got) != toJSON(tc.want) {
			t.Errorf("%s: %d steps %v, want %v (%s)", tc.name, status, got, tc.want, body)
		}
	}
}
