package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// switchingProber reports the target healthy or not, as set.
type switchingProber struct {
	ok    bool
	calls int
}

func (p *switchingProber) ProbeTarget(ctx context.Context, id string) (*registry.TargetHealth, []*registry.TargetAgent, error) {
	p.calls++
	h := &registry.TargetHealth{TargetID: id, ProbedAt: time.Now(), DiskFreeBytes: -1}
	if p.ok {
		h.Reachable, h.TmuxVersion = true, "tmux 3.5"
	} else {
		h.Error = healthReason(targets.SSHAuthFailed)
	}
	return h, nil, nil
}

func migrationPlan(t *testing.T) *targets.SSHConfigPlan {
	t.Helper()
	k, err := targets.GenerateSSHKey("", "imported-id_ed25519")
	if err != nil {
		t.Fatal(err)
	}
	k.ID, k.Origin = "", registry.SSHKeyOriginImported
	return &targets.SSHConfigPlan{Host: "wyzer.tail1234.ts.net", Port: 22, User: "orski", KeyFile: "/home/loomux/.ssh/id_ed25519",
		Key: k, HostKeys: "wyzer.tail1234.ts.net " + strings.TrimPrefix(testHostKeyLine, strings.Fields(testHostKeyLine)[0]+" ")}
}

type migrateResponse struct {
	CanApply   bool     `json:"can_apply"`
	Applied    bool     `json:"applied"`
	RolledBack bool     `json:"rolled_back"`
	Problems   []string `json:"problems"`
	Plan       struct {
		Host     string `json:"host"`
		SSHPort  int    `json:"ssh_port"`
		User     string `json:"user"`
		SSHProxy string `json:"ssh_proxy"`
		Key      struct {
			Fingerprint   string `json:"fingerprint"`
			SourceFile    string `json:"source_file"`
			ExistingKeyID string `json:"existing_key_id"`
		} `json:"key"`
		HostKeys []struct{ Fingerprint string } `json:"host_keys"`
	} `json:"plan"`
	Test *struct {
		Steps []struct{ Name, Status string }
	} `json:"test"`
	Target *managedTargetJSON `json:"target"`
}

func migrateServer(t *testing.T, plan *targets.SSHConfigPlan, prober api.TargetProber) (base, token string, store registry.Store, targetID string) {
	t.Helper()
	resolve := func(context.Context, *registry.Target) (*targets.SSHConfigPlan, error) {
		cp := *plan
		if plan.Key != nil {
			k := *plan.Key
			cp.Key = &k
		}
		return &cp, nil
	}
	base, token, store, _ = managedServerWith(t, testMasterKey, func(s registry.Store) []api.Option {
		return []api.Option{api.WithSSHMigration(resolve), api.WithTargetProber(prober), api.WithHostKeyPinning(nil, s)}
	})
	_, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "wyzer", "kind": "remote", "host": "wyzer", "user": "orski"})
	return base, token, store, decodeTarget(t, body).ID
}

func migrate(t *testing.T, base, token, id string, dryRun bool) (int, migrateResponse, string) {
	t.Helper()
	status, body := doJSON(t, "POST", base+"/api/v1/targets/"+id+"/migrate-ssh", token, map[string]bool{"dry_run": dryRun})
	var out migrateResponse
	_ = json.Unmarshal([]byte(body), &out)
	return status, out, body
}

// LOOM-138: a dry run shows what the migration would do and changes
// nothing; never the private key.
func TestMigrateSSH_DryRun(t *testing.T) {
	plan := migrationPlan(t)
	base, token, store, id := migrateServer(t, plan, &switchingProber{ok: true})
	status, out, body := migrate(t, base, token, id, true)
	if status != http.StatusOK || !out.CanApply || out.Applied || out.Plan.Host != plan.Host || out.Plan.User != "orski" ||
		out.Plan.SSHProxy != "default" || out.Plan.Key.Fingerprint != plan.Key.Fingerprint || out.Plan.Key.SourceFile != plan.KeyFile ||
		len(out.Plan.HostKeys) != 1 {
		t.Fatalf("dry run: %d %s", status, body)
	}
	assertNoPrivateKey(t, "dry run", body, plan.Key)
	got, _ := store.GetTarget(context.Background(), id)
	if got.Managed() || got.Host != "wyzer" {
		t.Errorf("dry run changed the target: %+v", got)
	}
	if keys, _ := store.ListSSHKeys(context.Background()); len(keys) != 0 {
		t.Errorf("dry run stored a key: %+v", keys)
	}
}

func TestMigrateSSH_RefusesWithProblems(t *testing.T) {
	plan := migrationPlan(t)
	plan.Problems = []string{"the SSH config reaches it through ProxyJump bastion"}
	base, token, store, id := migrateServer(t, plan, &switchingProber{ok: true})
	status, out, body := migrate(t, base, token, id, false)
	if status != http.StatusConflict || out.CanApply || out.Applied || len(out.Problems) != 1 {
		t.Errorf("apply with problems: %d %s", status, body)
	}
	if got, _ := store.GetTarget(context.Background(), id); got.Managed() {
		t.Error("target migrated despite problems")
	}
}

// Applied, the target is managed with the imported key and the pin, and
// tested; the key is imported once, however many targets share it.
func TestMigrateSSH_Apply(t *testing.T) {
	plan := migrationPlan(t)
	prober := &switchingProber{ok: true}
	base, token, store, id := migrateServer(t, plan, prober)
	ctx := context.Background()
	status, out, body := migrate(t, base, token, id, false)
	if status != http.StatusOK || !out.Applied || out.Target == nil || out.Target.SSHMode != "managed" || out.Target.Host != plan.Host ||
		out.Test == nil || prober.calls != 1 {
		t.Fatalf("apply: %d %s", status, body)
	}
	got, _ := store.GetTarget(ctx, id)
	if !got.Managed() || got.HostKeys == "" || got.User != "orski" || got.SSHProxy != "" {
		t.Errorf("stored = %+v", got)
	}
	k, err := store.GetSSHKey(ctx, got.SSHKeyRef)
	if err != nil || k.Origin != registry.SSHKeyOriginImported || k.Fingerprint != plan.Key.Fingerprint {
		t.Fatalf("imported key = %+v, %v", k, err)
	}

	_, body2 := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "wyzer2", "kind": "remote", "host": "wyzer2", "user": "orski"})
	id2 := decodeTarget(t, body2).ID
	_, dry, _ := migrate(t, base, token, id2, true)
	if dry.Plan.Key.ExistingKeyID != k.ID {
		t.Errorf("second target's plan doesn't reuse the imported key: %+v", dry.Plan.Key)
	}
	if status, _, body := migrate(t, base, token, id2, false); status != http.StatusOK {
		t.Fatalf("second apply: %d %s", status, body)
	}
	if keys, _ := store.ListSSHKeys(ctx); len(keys) != 1 {
		t.Errorf("%d keys after migrating two targets with one key file, want 1", len(keys))
	}
	if status, _, _ := migrate(t, base, token, id, true); status != http.StatusBadRequest {
		t.Errorf("migrating an already managed target: %d, want 400", status)
	}
}

// If the managed target doesn't work, it goes back to the SSH config as
// it was, and a key imported for it alone is removed.
func TestMigrateSSH_RollsBackWhenTheTestFails(t *testing.T) {
	plan := migrationPlan(t)
	base, token, store, id := migrateServer(t, plan, &switchingProber{ok: false})
	ctx := context.Background()
	before, _ := store.GetTarget(ctx, id)
	status, out, body := migrate(t, base, token, id, false)
	if status != http.StatusBadGateway || out.Applied || !out.RolledBack || out.Test == nil {
		t.Fatalf("failed apply: %d %s", status, body)
	}
	var auth string
	for _, s := range out.Test.Steps {
		if s.Name == "auth" {
			auth = s.Status
		}
	}
	if auth != "failed" {
		t.Errorf("test steps = %+v, want auth failed", out.Test.Steps)
	}
	after, _ := store.GetTarget(ctx, id)
	if after.Managed() || after.Host != before.Host || after.User != before.User || after.SSHPort != before.SSHPort || after.HostKeys != before.HostKeys {
		t.Errorf("after rollback = %+v, want %+v", after, before)
	}
	if keys, _ := store.ListSSHKeys(ctx); len(keys) != 0 {
		t.Errorf("rolled back, but the imported key stayed: %+v", keys)
	}
}

func TestMigrateSSH_NotConfigured(t *testing.T) {
	base, token, _, _ := managedServer(t, testMasterKey)
	_, body := doJSON(t, "POST", base+"/api/v1/targets", token, map[string]any{"name": "w", "kind": "remote", "host": "w", "user": "u"})
	id := decodeTarget(t, body).ID
	if status, _, _ := migrate(t, base, token, id, true); status != http.StatusNotImplemented {
		t.Errorf("without WithSSHMigration: %d, want 501", status)
	}
}
