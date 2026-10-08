package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

// sshKeyServer is a test server whose /ssh-keys store has a master key
// (or none, when masterKey is nil).
func sshKeyServer(t *testing.T, masterKey []byte) (baseURL, token string, store registry.Store) {
	t.Helper()
	var opts []sqlite.Option
	if masterKey != nil {
		opts = append(opts, sqlite.WithMasterKey(masterKey))
	}
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "keys.db"), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv, _, _ := newTestServer(t, api.WithSSHKeys(store))
	token, _ = login(t, srv.URL, testPassword)
	return srv.URL, token, store
}

type sshKeyJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Fingerprint string   `json:"fingerprint"`
	PublicKey   string   `json:"public_key"`
	Origin      string   `json:"origin"`
	UsedBy      []string `json:"used_by"`
}

// assertNoPrivateKey fails if body carries any part of k's private key.
func assertNoPrivateKey(t *testing.T, what, body string, k *registry.SSHKey) {
	t.Helper()
	if strings.Contains(body, "PRIVATE KEY") || strings.Contains(strings.ToLower(body), "private") {
		t.Errorf("%s mentions a private key: %s", what, body)
	}
	for _, line := range strings.Split(string(k.PrivateKey), "\n") {
		if len(line) > 16 && !strings.HasPrefix(line, "-----") && strings.Contains(body, line) {
			t.Errorf("%s contains private key material: %s", what, body)
		}
	}
}

// LOOM-138: generate, list and delete managed SSH keys. Only the public
// parts ever come back.
func TestSSHKeys_GenerateListDelete(t *testing.T) {
	base, token, store := sshKeyServer(t, bytes.Repeat([]byte("k"), 32))
	ctx := context.Background()

	status, body := doJSON(t, "POST", base+"/api/v1/ssh-keys", token, map[string]string{"name": "wyzer"})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	var created sshKeyJSON
	_ = json.Unmarshal([]byte(body), &created)
	if created.ID == "" || created.Name != "wyzer" || created.Type != "ssh-ed25519" || created.Origin != "generated" ||
		!strings.HasSuffix(created.PublicKey, " loomux-wyzer") || created.UsedBy == nil {
		t.Fatalf("created = %+v", created)
	}
	stored, err := store.GetSSHKey(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(stored.PrivateKey)
	if err != nil || ssh.FingerprintSHA256(signer.PublicKey()) != created.Fingerprint {
		t.Fatalf("stored private key doesn't match the shown fingerprint: %v", err)
	}
	assertNoPrivateKey(t, "create", body, stored)

	status, body = doJSON(t, "GET", base+"/api/v1/ssh-keys", token, nil)
	var list struct {
		SSHKeys []sshKeyJSON `json:"ssh_keys"`
	}
	_ = json.Unmarshal([]byte(body), &list)
	if status != http.StatusOK || len(list.SSHKeys) != 1 || list.SSHKeys[0].Fingerprint != created.Fingerprint {
		t.Fatalf("list: %d %s", status, body)
	}
	assertNoPrivateKey(t, "list", body, stored)

	if status, body = doJSON(t, "POST", base+"/api/v1/ssh-keys", token, map[string]string{"name": "wyzer"}); status != http.StatusConflict {
		t.Errorf("duplicate name: %d %s, want 409", status, body)
	}
	if status, body = doJSON(t, "DELETE", base+"/api/v1/ssh-keys/"+created.ID, token, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", status, body)
	}
	if status, _ = doJSON(t, "DELETE", base+"/api/v1/ssh-keys/"+created.ID, token, nil); status != http.StatusNotFound {
		t.Errorf("delete again: %d, want 404", status)
	}
}

// A name goes into the public key's comment, on the line a person pastes
// into authorized_keys: nothing that could split it or add options.
func TestSSHKeys_RefusesBadNames(t *testing.T) {
	base, token, _ := sshKeyServer(t, bytes.Repeat([]byte("k"), 32))
	for _, name := range []string{"", " ", "-o", "a b", "x\nssh-ed25519 AAAA attacker", `command="id" x`, "a/b", "../x", strings.Repeat("a", 65)} {
		if status, body := doJSON(t, "POST", base+"/api/v1/ssh-keys", token, map[string]string{"name": name}); status != http.StatusBadRequest {
			t.Errorf("name %q: %d %s, want 400", name, status, body)
		}
	}
}

func TestSSHKeys_InUseByATarget(t *testing.T) {
	base, token, store := sshKeyServer(t, bytes.Repeat([]byte("k"), 32))
	_, body := doJSON(t, "POST", base+"/api/v1/ssh-keys", token, map[string]string{"name": "wyzer"})
	var created sshKeyJSON
	_ = json.Unmarshal([]byte(body), &created)
	target := &registry.Target{ID: "t-1", Name: "wyzer", Kind: registry.TargetKindRemote, Host: "wyzer", User: "orski", SSHKeyRef: created.ID}
	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	_, body = doJSON(t, "GET", base+"/api/v1/ssh-keys", token, nil)
	if !strings.Contains(body, `"used_by":["t-1"]`) {
		t.Errorf("list doesn't show the target using the key: %s", body)
	}
	if status, body := doJSON(t, "DELETE", base+"/api/v1/ssh-keys/"+created.ID, token, nil); status != http.StatusConflict {
		t.Errorf("delete in use: %d %s, want 409", status, body)
	}
}

func TestSSHKeys_NoMasterKey(t *testing.T) {
	base, token, _ := sshKeyServer(t, nil)
	status, body := doJSON(t, "POST", base+"/api/v1/ssh-keys", token, map[string]string{"name": "wyzer"})
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "LOOMUX_MASTER_KEY") {
		t.Errorf("create without a master key: %d %s, want 503 naming LOOMUX_MASTER_KEY", status, body)
	}
	if status, body := doJSON(t, "GET", base+"/api/v1/ssh-keys", token, nil); status != http.StatusOK {
		t.Errorf("list without a master key: %d %s", status, body)
	}
}

func TestSSHKeys_AuthAndConfiguration(t *testing.T) {
	base, _, _ := sshKeyServer(t, bytes.Repeat([]byte("k"), 32))
	for _, r := range []struct{ method, path string }{{"GET", "/api/v1/ssh-keys"}, {"POST", "/api/v1/ssh-keys"}, {"DELETE", "/api/v1/ssh-keys/x"}} {
		if status, _ := doJSON(t, r.method, base+r.path, "", map[string]string{"name": "x"}); status != http.StatusUnauthorized {
			t.Errorf("%s %s without a session: %d, want 401", r.method, r.path, status)
		}
	}
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	if status, _ := doJSON(t, "GET", srv.URL+"/api/v1/ssh-keys", token, nil); status != http.StatusNotFound {
		t.Errorf("without WithSSHKeys: %d, want 404", status)
	}
}
