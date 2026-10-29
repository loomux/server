package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// LOOM-248: eight POST /api/v1/workspaces at once on one local target
// all provision, while another connection reads. With the database in
// rollback-journal mode a reader blocks every commit, and on TEST one of
// eight failed: "launch: sqlite: update workspace: database is locked
// (5) (SQLITE_BUSY)".
func TestProvisionWorkspace_ConcurrentPosts(t *testing.T) {
	const n = 8
	srv := fakeRouterServer(t)
	cfg := testConfig(t, srv.URL)
	a, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx := context.Background()
	target := &registry.Target{ID: uuid.NewString(), Name: "box", Kind: registry.TargetKindLocal, WorkspaceRoot: t.TempDir()}
	if err := a.store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}

	const password = "correct-horse-battery-staple"
	hash, err := api.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	st := a.store
	httpSrv := httptest.NewServer(api.NewServer(a.Dispatches(), st, st, st, st, st, st, []byte(hash),
		api.WithLocalTargets(true), api.WithWorkspaceProvisioner(a)))
	t.Cleanup(httpSrv.Close)
	token := apiLogin(t, httpSrv.URL, password)

	// A reader holds its snapshot across the whole burst, as a slow list,
	// a backup or the sqlite3 shell reading the file does. In the
	// rollback journal that blocked every write past the busy timeout;
	// in WAL, writes go on beside it.
	reader, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	snapshot, err := reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Rollback() }()
	var before int
	if err := snapshot.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			body, _ := json.Marshal(map[string]string{"target_id": target.ID, "name": fmt.Sprintf("ws-%d", i), "kind": "empty"})
			req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/workspaces", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("POST %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			var out struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode/100 != 2 {
				t.Errorf("POST %d: %s, %v", i, resp.Status, err)
				return
			}
			ids[i] = out.ID
		})
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	deadline := time.Now().Add(60 * time.Second)
	for _, id := range ids {
		for {
			ws, err := a.store.GetWorkspace(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if ws.Status == registry.WorkspaceStatusProvisioning {
				if time.Now().After(deadline) {
					t.Fatalf("%s still provisioning after 60s", ws.Name)
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if ws.Status != registry.WorkspaceStatusIdle {
				t.Errorf("%s ended %s: %s", ws.Name, ws.Status, ws.StatusReason)
			}
			break
		}
	}
}

// apiLogin logs in to the API at baseURL and returns the session token.
func apiLogin(t *testing.T, baseURL, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	resp, err := http.Post(baseURL+"/api/v1/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		t.Fatalf("login: %s, %v", resp.Status, err)
	}
	return out.Token
}
