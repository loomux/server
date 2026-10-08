package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

const testHostKeyLine = "box ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// LOOM-114: a scan trusts nothing; a pin must name a fingerprint the
// target's latest scan returned; unpin goes back to the mounted
// known_hosts.
func TestHostKeyScanAndPin(t *testing.T) {
	scanned, _ := targets.ParseHostKeys(testHostKeyLine)
	var scanErr error
	scan := func(ctx context.Context, tgt *registry.Target) ([]targets.HostKey, error) { return scanned, scanErr }
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option {
		return []api.Option{api.WithHostKeyPinning(scan, s)}
	})
	ctx := context.Background()
	for _, tgt := range []*registry.Target{
		{ID: "remote", Name: "box", Kind: registry.TargetKindRemote, Host: "box", User: "u"},
		{ID: "local", Name: "here", Kind: registry.TargetKindLocal},
	} {
		if err := store.CreateTarget(ctx, tgt); err != nil {
			t.Fatalf("CreateTarget: %v", err)
		}
	}
	token, _ := login(t, srv.URL, testPassword)
	post := func(path string, body any) (*http.Response, map[string]any) {
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		resp := authedRequest(t, http.MethodPost, srv.URL+path, token, raw)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	fp := scanned[0].Fingerprint

	if resp, _ := post("/api/v1/targets/remote/pin", map[string]string{"fingerprint": fp}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("pin before a scan = %d, want 409", resp.StatusCode)
	}
	if resp, _ := post("/api/v1/targets/local/scan-host-key", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("scan of a local target = %d, want 400", resp.StatusCode)
	}
	resp, body := post("/api/v1/targets/remote/scan-host-key", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(toJSON(body["host_keys"]), fp) {
		t.Fatalf("scan = %d %v, want the fingerprint", resp.StatusCode, body)
	}
	if got, _ := store.GetTarget(ctx, "remote"); got.HostKeys != "" {
		t.Fatalf("a scan pinned %q; nothing is trusted until a pin", got.HostKeys)
	}
	if resp, _ := post("/api/v1/targets/remote/pin", map[string]string{"fingerprint": "SHA256:somethingelse"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("pin of another fingerprint = %d, want 409", resp.StatusCode)
	}
	resp, body = post("/api/v1/targets/remote/pin", map[string]string{"fingerprint": fp})
	if resp.StatusCode != http.StatusOK || !strings.Contains(toJSON(body["pinned_host_keys"]), fp) {
		t.Fatalf("pin = %d %v, want the target with its pinned key", resp.StatusCode, body)
	}
	if got, _ := store.GetTarget(ctx, "remote"); got.HostKeys != testHostKeyLine {
		t.Fatalf("stored pin = %q", got.HostKeys)
	}

	unpin := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/targets/remote/pin", token, nil)
	unpin.Body.Close()
	if got, _ := store.GetTarget(ctx, "remote"); unpin.StatusCode != http.StatusOK || got.HostKeys != "" {
		t.Fatalf("unpin = %d, stored %q", unpin.StatusCode, got.HostKeys)
	}

	// The port is a per-target override set like any other field, and
	// an update leaves the pin alone.
	raw, _ := json.Marshal(map[string]any{"name": "box", "kind": "remote", "host": "box", "user": "u", "ssh_port": 2222})
	if err := store.SetTargetHostKeys(ctx, "remote", testHostKeyLine); err != nil {
		t.Fatalf("SetTargetHostKeys: %v", err)
	}
	put := authedRequest(t, http.MethodPut, srv.URL+"/api/v1/targets/remote", token, raw)
	put.Body.Close()
	if got, _ := store.GetTarget(ctx, "remote"); put.StatusCode != http.StatusOK || got.SSHPort != 2222 || got.HostKeys != testHostKeyLine {
		t.Fatalf("PUT ssh_port = %d, stored %+v; want port 2222 and the pin kept", put.StatusCode, got)
	}
	raw, _ = json.Marshal(map[string]any{"name": "box", "kind": "remote", "host": "box", "user": "u", "ssh_port": 70000})
	bad := authedRequest(t, http.MethodPut, srv.URL+"/api/v1/targets/remote", token, raw)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT ssh_port 70000 = %d, want 400", bad.StatusCode)
	}

	scanErr = &targets.UnreachableError{Host: "box", Failure: targets.SSHConnectionRefused}
	if resp, body := post("/api/v1/targets/remote/scan-host-key", nil); resp.StatusCode != http.StatusBadGateway ||
		!strings.Contains(toJSON(body), "refused") {
		t.Fatalf("failed scan = %d %v, want 502 with the hint", resp.StatusCode, body)
	}
}

// LOOM-114: test reports reachability and tmux, and flags a host key
// problem for the re-pin flow.
func TestTestTarget_FlagsHostKeyProblem(t *testing.T) {
	hk := &targets.UnreachableError{Host: "box", Failure: targets.SSHHostKeyChanged}
	prober := &fakeTargetProber{health: &registry.TargetHealth{TargetID: "remote", Error: hk.Error()}}
	srv, _, _ := newTestServer(t, api.WithTargetProber(prober))
	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/remote/test", token, nil)
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusOK || body["reachable"] != false || body["host_key_problem"] != true ||
		!strings.Contains(body["error"].(string), "scan and pin") {
		t.Fatalf("test = %d %v, want unreachable with a host key problem and the re-pin hint", resp.StatusCode, body)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// LOOM-164: a scan still running when a PUT moves the target lands after
// the update forgot the old scan; its keys came from the old address and
// must not be pinnable for the new one.
func TestHostKeyScan_InFlightAcrossAddressChangeNotPinnable(t *testing.T) {
	scanned, _ := targets.ParseHostKeys(testHostKeyLine)
	entered, release := make(chan struct{}), make(chan struct{})
	scan := func(ctx context.Context, tgt *registry.Target) ([]targets.HostKey, error) {
		close(entered)
		<-release
		return scanned, nil
	}
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option {
		return []api.Option{api.WithHostKeyPinning(scan, s)}
	})
	ctx := context.Background()
	if err := store.CreateTarget(ctx, &registry.Target{ID: "remote", Name: "box", Kind: registry.TargetKindRemote, Host: "old-box", User: "u"}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	token, _ := login(t, srv.URL, testPassword)

	scanDone := make(chan int, 1)
	go func() {
		resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/remote/scan-host-key", token, nil)
		resp.Body.Close()
		scanDone <- resp.StatusCode
	}()
	<-entered
	raw, _ := json.Marshal(map[string]any{"name": "box", "kind": "remote", "host": "new-box", "user": "u"})
	put := authedRequest(t, http.MethodPut, srv.URL+"/api/v1/targets/remote", token, raw)
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Fatalf("PUT host = %d", put.StatusCode)
	}
	close(release)
	if status := <-scanDone; status != http.StatusOK {
		t.Fatalf("scan = %d", status)
	}

	pinRaw, _ := json.Marshal(map[string]string{"fingerprint": scanned[0].Fingerprint})
	pin := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/remote/pin", token, pinRaw)
	pin.Body.Close()
	if pin.StatusCode != http.StatusConflict {
		t.Fatalf("pin of old-box's scan after the move to new-box = %d, want 409", pin.StatusCode)
	}
	if got, _ := store.GetTarget(ctx, "remote"); got.HostKeys != "" {
		t.Fatalf("old address's key pinned for the new one: %q", got.HostKeys)
	}
}

// movingHostKeyStore moves the target to another host just before the
// first pin is written: a PUT racing the pin.
type movingHostKeyStore struct {
	registry.Store
	moved bool
}

func (m *movingHostKeyStore) SetTargetHostKeys(ctx context.Context, id, hostKeys string) error {
	if !m.moved {
		m.moved = true
		tgt, err := m.Store.GetTarget(ctx, id)
		if err != nil {
			return err
		}
		tgt.Host = "elsewhere"
		if err := m.Store.UpdateTarget(ctx, tgt); err != nil {
			return err
		}
	}
	return m.Store.SetTargetHostKeys(ctx, id, hostKeys)
}

// LOOM-164: a PUT moving the target while a pin is written leaves no pin
// for the new address.
func TestHostKeyPin_RacingAddressChangeIsUndone(t *testing.T) {
	scanned, _ := targets.ParseHostKeys(testHostKeyLine)
	scan := func(ctx context.Context, tgt *registry.Target) ([]targets.HostKey, error) { return scanned, nil }
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option {
		return []api.Option{api.WithHostKeyPinning(scan, &movingHostKeyStore{Store: s})}
	})
	ctx := context.Background()
	if err := store.CreateTarget(ctx, &registry.Target{ID: "remote", Name: "box", Kind: registry.TargetKindRemote, Host: "box", User: "u"}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	token, _ := login(t, srv.URL, testPassword)
	scanResp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/remote/scan-host-key", token, nil)
	scanResp.Body.Close()
	pinRaw, _ := json.Marshal(map[string]string{"fingerprint": scanned[0].Fingerprint})
	pin := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets/remote/pin", token, pinRaw)
	pin.Body.Close()
	if pin.StatusCode != http.StatusConflict {
		t.Fatalf("pin racing a move = %d, want 409", pin.StatusCode)
	}
	if got, _ := store.GetTarget(ctx, "remote"); got.Host != "elsewhere" || got.HostKeys != "" {
		t.Fatalf("after the race: host %q, pin %q; want elsewhere and no pin", got.Host, got.HostKeys)
	}
}
