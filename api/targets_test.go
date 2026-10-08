package api_test

import (
	"context"
	"encoding/json"
	"github.com/Loomux/server/api"
	"net/http"
	"testing"

	"github.com/Loomux/server/registry"
)

// targetJSON mirrors the API's target representation, as a client would
// have to model it.
type targetJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Host      string `json:"host"`
	User      string `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// createTarget POSTs body to /api/v1/targets and returns the decoded
// response alongside its status, so a test can assert on either.
func createTarget(t *testing.T, baseURL, token string, body map[string]any) (targetJSON, int) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp := authedRequest(t, http.MethodPost, baseURL+"/api/v1/targets", token, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return targetJSON{}, resp.StatusCode
	}
	var out targetJSON
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return out, resp.StatusCode
}

// assertErrorEnvelope checks the response carries the API's JSON error
// body rather than net/http's plain-text mux 404.
func assertErrorEnvelope(t *testing.T, resp *http.Response) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error == "" {
		t.Error("error body is empty, want an explanatory message")
	}
}

func listTargets(t *testing.T, baseURL, token string) ([]targetJSON, int) {
	t.Helper()
	resp := authedRequest(t, http.MethodGet, baseURL+"/api/v1/targets", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	var out struct {
		Targets []targetJSON `json:"targets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	return out.Targets, resp.StatusCode
}

// TestCreateTarget_Remote_PersistsAndReadsBack is the core of LOOM-59:
// before this endpoint existed, registry.Store's target CRUD had no
// production caller at all, so the only way to register a target was a
// raw sqlite INSERT on the deployment host. Asserts the round trip goes
// all the way to the store and back out through the list endpoint.
func TestCreateTarget_Remote_PersistsAndReadsBack(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	created, status := createTarget(t, srv.URL, token, map[string]any{
		"name":        "sc1",
		"kind":        "remote",
		"host":        "sc1.example.net",
		"user":        "loomux",
		"ssh_key_ref": "vault:sc1-key",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	if created.ID == "" {
		t.Error("id is empty, want a server-generated id")
	}
	if created.Name != "sc1" || created.Kind != "remote" || created.Host != "sc1.example.net" || created.User != "loomux" {
		t.Errorf("created = %+v, want the submitted field values", created)
	}
	// ssh_key_ref is no longer part of the API (freeze review item 1): a
	// client still sending it is ignored, not refused.
	if stored, _ := store.GetTarget(context.Background(), created.ID); stored != nil && stored.SSHKeyRef != "" {
		t.Errorf("stored ssh_key_ref = %q, want it ignored", stored.SSHKeyRef)
	}

	stored, err := store.GetTarget(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTarget(%q): %v", created.ID, err)
	}
	if stored.Name != "sc1" || stored.Kind != registry.TargetKindRemote {
		t.Errorf("stored = %+v, want the created target", stored)
	}

	listed, status := listTargets(t, srv.URL, token)
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed = %+v, want exactly the created target", listed)
	}
}

func TestCreateTarget_Local_OmitsHostAndUser(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	created, status := createTarget(t, srv.URL, token, map[string]any{
		"name": "localhost",
		"kind": "local",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	if created.Host != "" || created.User != "" {
		t.Errorf("host/user = %q/%q, want both empty for a local target", created.Host, created.User)
	}
}

func TestCreateTarget_ServerGeneratesDistinctIDs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	first, _ := createTarget(t, srv.URL, token, map[string]any{"name": "a", "kind": "local"})
	second, _ := createTarget(t, srv.URL, token, map[string]any{"name": "b", "kind": "local"})
	if first.ID == second.ID {
		t.Errorf("both targets got id %q, want distinct server-generated ids", first.ID)
	}
}

// TestCreateTarget_ClientSuppliedID_IsIgnored keeps id server-owned: a
// client that guesses at the field must not be able to pin or collide
// with an id workspaces will later reference.
func TestCreateTarget_ClientSuppliedID_IsIgnored(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	created, status := createTarget(t, srv.URL, token, map[string]any{
		"id": "client-chosen", "name": "a", "kind": "local",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	if created.ID == "client-chosen" {
		t.Error("id = client-chosen, want the server's own id")
	}
}

func TestCreateTarget_MissingName_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	if _, status := createTarget(t, srv.URL, token, map[string]any{"kind": "local"}); status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// TestCreateTarget_UnknownKind_ReturnsBadRequest guards targets.NewExecutor,
// which errors on any kind outside local/remote — a target stored with
// one would be permanently undispatchable.
func TestCreateTarget_UnknownKind_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	for _, kind := range []string{"", "container", "LOCAL"} {
		if _, status := createTarget(t, srv.URL, token, map[string]any{"name": "x", "kind": kind}); status != http.StatusBadRequest {
			t.Errorf("kind %q: status = %d, want 400", kind, status)
		}
	}
}

// TestCreateTarget_RemoteWithoutUser_ReturnsBadRequest covers
// targets.RemoteExecutor.destination(), which builds user+"@"+host — an
// empty user yields "@host", which ssh rejects, so the target would be
// stored but unusable.
func TestCreateTarget_RemoteWithoutUser_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	if _, status := createTarget(t, srv.URL, token, map[string]any{
		"name": "x", "kind": "remote", "host": "h.example.net",
	}); status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestCreateTarget_RemoteWithoutHost_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	if _, status := createTarget(t, srv.URL, token, map[string]any{
		"name": "x", "kind": "remote", "user": "loomux",
	}); status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// TestCreateTarget_LocalWithHostOrUser_ReturnsBadRequest rejects rather
// than silently clearing: a caller who sent host/user for a local target
// has misunderstood something, and quietly dropping the fields would
// hide that until an attach-info response came back missing them.
func TestCreateTarget_LocalWithHostOrUser_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	if _, status := createTarget(t, srv.URL, token, map[string]any{
		"name": "x", "kind": "local", "host": "h.example.net",
	}); status != http.StatusBadRequest {
		t.Errorf("with host: status = %d, want 400", status)
	}
	if _, status := createTarget(t, srv.URL, token, map[string]any{
		"name": "y", "kind": "local", "user": "loomux",
	}); status != http.StatusBadRequest {
		t.Errorf("with user: status = %d, want 400", status)
	}
}

func TestCreateTarget_MalformedBody_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets", token, []byte("{not json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestCreateTarget_DuplicateName_ReturnsConflict surfaces the store's
// own unique-name constraint as a 409 rather than a 500.
func TestCreateTarget_DuplicateName_ReturnsConflict(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	if _, status := createTarget(t, srv.URL, token, map[string]any{"name": "dup", "kind": "local"}); status != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", status)
	}
	if _, status := createTarget(t, srv.URL, token, map[string]any{"name": "dup", "kind": "local"}); status != http.StatusConflict {
		t.Errorf("second create status = %d, want 409", status)
	}
}

func TestListTargets_Empty_ReturnsEmptyArray(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/targets", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Targets []targetJSON `json:"targets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Targets == nil {
		t.Error("targets = null, want [] so clients can iterate unconditionally")
	}
}

func TestListTargets_SortedByName(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	for _, name := range []string{"zeta", "alpha", "middle"} {
		if _, status := createTarget(t, srv.URL, token, map[string]any{"name": name, "kind": "local"}); status != http.StatusCreated {
			t.Fatalf("create %q: status = %d, want 201", name, status)
		}
	}

	listed, _ := listTargets(t, srv.URL, token)
	got := []string{}
	for _, tgt := range listed {
		got = append(got, tgt.Name)
	}
	want := []string{"alpha", "middle", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}
}

// TestUpdateTarget_ChangesStoredFields covers the only correction path
// available once workspaces reference a target: DeleteTarget refuses
// with a conflict at that point, so update is how an operator fixes a
// wrong host/user without shell access to the deployment.
func TestUpdateTarget_ChangesStoredFields(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	created, _ := createTarget(t, srv.URL, token, map[string]any{
		"name": "sc1", "kind": "remote", "host": "old.example.net", "user": "olduser",
	})

	body, _ := json.Marshal(map[string]any{
		"name": "sc1", "kind": "remote", "host": "new.example.net", "user": "newuser",
	})
	resp := authedRequest(t, http.MethodPut, srv.URL+"/api/v1/targets/"+created.ID, token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var updated targetJSON
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.ID != created.ID {
		t.Errorf("id = %q, want unchanged %q", updated.ID, created.ID)
	}
	if updated.Host != "new.example.net" || updated.User != "newuser" {
		t.Errorf("updated = %+v, want the new host/user", updated)
	}

	stored, err := store.GetTarget(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	if stored.Host != "new.example.net" || stored.User != "newuser" {
		t.Errorf("stored = %+v, want the new host/user", stored)
	}
}

func TestUpdateTarget_UnknownID_ReturnsNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	body, _ := json.Marshal(map[string]any{"name": "x", "kind": "local"})
	resp := authedRequest(t, http.MethodPut, srv.URL+"/api/v1/targets/nope", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	// A registered route answers with the API's JSON error envelope; an
	// unregistered one answers with net/http's plain-text 404. Assert the
	// envelope so this test can only pass once the route exists.
	assertErrorEnvelope(t, resp)
}

func TestUpdateTarget_InvalidBody_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	created, _ := createTarget(t, srv.URL, token, map[string]any{"name": "a", "kind": "local"})

	body, _ := json.Marshal(map[string]any{"name": "a", "kind": "remote"})
	resp := authedRequest(t, http.MethodPut, srv.URL+"/api/v1/targets/"+created.ID, token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (remote without host/user)", resp.StatusCode)
	}
}

func TestDeleteTarget_RemovesIt(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	created, _ := createTarget(t, srv.URL, token, map[string]any{"name": "doomed", "kind": "local"})

	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/targets/"+created.ID, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	if _, err := store.GetTarget(context.Background(), created.ID); err == nil {
		t.Error("GetTarget succeeded after delete, want ErrNotFound")
	}
}

func TestDeleteTarget_UnknownID_ReturnsNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/targets/nope", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	assertErrorEnvelope(t, resp)
}

// TestDeleteTarget_WithWorkspaces_ReturnsConflict surfaces the store's
// foreign-key refusal as a 409, so an operator is told why rather than
// seeing a generic 500.
func TestDeleteTarget_WithWorkspaces_ReturnsConflict(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	created, _ := createTarget(t, srv.URL, token, map[string]any{"name": "busy", "kind": "local"})
	ws := &registry.Workspace{ID: "ws-busy", Name: "busy-ws", Path: "/tmp/busy", TargetID: created.ID, Status: registry.WorkspaceStatusActive}
	if err := store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/targets/"+created.ID, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// TestTargets_NoToken_ReturnsUnauthorized keeps every target route
// behind the same auth gate as the rest of /api/v1 — registering a
// target is an administrative act, and an unauthenticated caller must
// not be able to enumerate hosts and usernames either.
func TestTargets_NoToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/targets"},
		{http.MethodPost, "/api/v1/targets"},
		{http.MethodPut, "/api/v1/targets/some-id"},
		{http.MethodDelete, "/api/v1/targets/some-id"},
	}
	for _, tc := range cases {
		resp := authedRequest(t, tc.method, srv.URL+tc.path, "", []byte(`{"name":"x","kind":"local"}`))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestCreateTarget_NoToken_DoesNotWrite proves the auth gate blocks the
// write itself, not merely the response.
func TestCreateTarget_NoToken_DoesNotWrite(t *testing.T) {
	srv, _, store := newTestServer(t)

	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets", "", []byte(`{"name":"sneaky","kind":"local"}`))
	resp.Body.Close()

	targets, err := store.ListTargets(context.Background())
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("store has %d targets, want 0 — unauthenticated request must not write", len(targets))
	}
}

// TestCreateTarget_WorkspaceRoot (LOOM-90): the root dynamic workspaces
// are confined to round-trips, and one that isn't an absolute, clean path
// is rejected at registration.
func TestCreateTarget_WorkspaceRoot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets", token,
		[]byte(`{"name":"jet01","kind":"local","workspace_root":"/srv/loomux"}`))
	defer resp.Body.Close()
	var created struct {
		WorkspaceRoot string `json:"workspace_root"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, %v", resp.StatusCode, err)
	}
	if created.WorkspaceRoot != "/srv/loomux" {
		t.Errorf("workspace_root = %q, want it round-tripped", created.WorkspaceRoot)
	}

	for _, root := range []string{"~", "relative", "/srv/../etc", "/"} {
		bad := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/targets", token,
			[]byte(`{"name":"x-`+root+`","kind":"local","workspace_root":"`+root+`"}`))
		if bad.StatusCode != http.StatusBadRequest {
			t.Errorf("workspace_root %q: status %d, want 400", root, bad.StatusCode)
		}
		bad.Body.Close()
	}
}

// LOOM-119: a PUT that omits the optional workspace_root keeps what's
// stored; an explicit "" clears it.
func TestUpdateTarget_OmittedOptionalFieldsKept(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	created, _ := createTarget(t, srv.URL, token, map[string]any{
		"name": "jet01", "kind": "remote", "host": "jet01.example.net", "user": "orski",
		"workspace_root": "/srv/work",
	})

	put := func(body map[string]any) *registry.Target {
		t.Helper()
		raw, _ := json.Marshal(body)
		resp := authedRequest(t, http.MethodPut, srv.URL+"/api/v1/targets/"+created.ID, token, raw)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status = %d", resp.StatusCode)
		}
		stored, err := store.GetTarget(context.Background(), created.ID)
		if err != nil {
			t.Fatalf("GetTarget: %v", err)
		}
		return stored
	}

	got := put(map[string]any{"name": "jet01", "kind": "remote", "host": "jet01.example.net", "user": "orski2"})
	if got.User != "orski2" || got.WorkspaceRoot != "/srv/work" {
		t.Errorf("after a PUT omitting it: workspace_root=%q user=%q, want kept", got.WorkspaceRoot, got.User)
	}
	got = put(map[string]any{"name": "jet01", "kind": "remote", "host": "jet01.example.net", "user": "orski2",
		"workspace_root": ""})
	if got.WorkspaceRoot != "" {
		t.Errorf("after a PUT with \"\": workspace_root=%q, want cleared", got.WorkspaceRoot)
	}
}

// LOOM-141: with local targets off, registering one (or turning a target
// into one) is refused with a plain reason; remote targets are fine.
func TestTargets_LocalTargetsOff(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithLocalTargets(false))
	token, _ := login(t, srv.URL, testPassword)
	if _, status := createTarget(t, srv.URL, token, map[string]any{"name": "box", "kind": "local"}); status != http.StatusBadRequest {
		t.Fatalf("create local target: %d, want 400", status)
	}
	remote, status := createTarget(t, srv.URL, token, map[string]any{"name": "r", "kind": "remote", "host": "h.example.net", "user": "loomux"})
	if status != http.StatusCreated {
		t.Fatalf("create remote target: %d, want 201", status)
	}
	resp := authedRequest(t, http.MethodPut, srv.URL+"/api/v1/targets/"+remote.ID, token, mustJSON(t, map[string]any{"name": "r", "kind": "local"}))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("change a target to local: %d, want 400", resp.StatusCode)
	}

	on, _, _ := newTestServer(t)
	onToken, _ := login(t, on.URL, testPassword)
	if _, status := createTarget(t, on.URL, onToken, map[string]any{"name": "box", "kind": "local"}); status != http.StatusCreated {
		t.Fatalf("create local target with the default (on): %d, want 201", status)
	}
}
