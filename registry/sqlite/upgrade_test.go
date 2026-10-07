package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/Loomux/server/registry"
)

// v010Schema is the last migration v0.1.0 shipped with.
const v010Schema = 18

//go:embed testdata/v0.1.0-seed.sql
var v010Seed string

// v020Schema is the last migration v0.2.0 shipped with: what production
// runs.
const v020Schema = 19

//go:embed testdata/v0.2.0-seed.sql
var v020Seed string

// seedAt makes dbPath a database as the release whose last migration is
// schema left it, holding seed.
func seedAt(t *testing.T, dbPath string, schema int64, seed string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	goose.SetTableName("schema_migrations")
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", schema); err != nil {
		t.Fatalf("goose.UpTo(%d): %v", schema, err)
	}
	if _, err := db.ExecContext(context.Background(), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestUpgradeFromV010 (LOOM-126): a database as v0.1.0 left it opens
// with today's code, every migration since applied, and everything in it
// reads back through today's store. A migration that breaks a deployed
// database fails here, not in production. When a release changes how
// existing rows are read, extend the checks; the seed stays as v0.1.0
// wrote it.
func TestUpgradeFromV010(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v010.db")

	seedAt(t, dbPath, v010Schema, v010Seed)

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (applies every migration after v0.1.0): %v", err)
	}
	defer store.Close()

	jet, err := store.GetTarget(ctx, "t-jet")
	if err != nil || jet.Name != "jet01" || jet.PermissionMode != "auto" {
		t.Errorf("target jet01 = %+v, %v", jet, err)
	}
	sc1, err := store.GetTarget(ctx, "t-work")
	if err != nil || sc1.Policy.Purpose != registry.TargetPurposeWork || !sc1.Policy.NoShell ||
		!sc1.Policy.RequireConfirmation || len(sc1.Policy.AllowedAgentTypes) != 1 {
		t.Errorf("target sc1 = %+v, %v; want its work policy kept", sc1, err)
	}
	if agents, err := store.ListTargetAgents(ctx, "t-jet"); err != nil || len(agents) != 1 || !agents[0].Available ||
		agents[0].AuthStatus != "logged_in" {
		t.Errorf("target agents = %+v, %v", agents, err)
	}
	if health, err := store.ListTargetHealth(ctx); err != nil || len(health) != 1 || !health[0].Reachable {
		t.Errorf("target health = %+v, %v", health, err)
	}

	ws, err := store.GetWorkspace(ctx, "w-1")
	if err != nil || ws.Status != registry.WorkspaceStatusIdle || ws.RollingSummary != "created hello.txt" ||
		len(ws.Tags) != 1 || ws.LastUsedAt == nil {
		t.Errorf("workspace = %+v, %v", ws, err)
	}

	cmd, err := store.GetTask(ctx, "k-cmd")
	if err != nil || cmd.Kind != registry.TaskKindCommand || cmd.Command != "df -h" || cmd.ExitCode == nil || *cmd.ExitCode != 0 {
		t.Errorf("command task = %+v, %v", cmd, err)
	}
	ask, err := store.GetTask(ctx, "k-ask")
	if err != nil || ask.Status != registry.TaskStatusNeedsAttention || ask.Attention == nil || len(ask.Attention.Options) != 2 {
		t.Errorf("needs-attention task = %+v, %v", ask, err)
	}
	if turns, err := store.ListTaskTurns(ctx, "k-agent"); err != nil || len(turns) != 1 || turns[0].AgentMessage != "Created hello.txt." {
		t.Errorf("task turns = %+v, %v", turns, err)
	}

	msgs, err := store.ListMessagesByConversation(ctx, "conv-1")
	if err != nil || len(msgs) != 3 || msgs[0].DispatchID != "d-1" || msgs[1].Origin != registry.TargetPurposePersonal ||
		msgs[2].TaskID != "k-cmd" {
		t.Errorf("messages = %+v, %v", msgs, err)
	}
	d, err := store.GetDispatch(ctx, "d-2")
	if err != nil || d.Status != registry.DispatchStatusSucceeded || d.ConfirmationID != "c-1" {
		t.Errorf("dispatch = %+v, %v", d, err)
	}
	if byKey, err := store.GetDispatchByIdempotencyKey(ctx, "idem-1"); err != nil || byKey.ID != "d-1" {
		t.Errorf("dispatch by idempotency key = %+v, %v", byKey, err)
	}
	confs, err := store.ListConfirmationsByConversation(ctx, "conv-1")
	if err != nil || len(confs) != 1 || confs[0].Status != registry.ConfirmationApproved || confs[0].Command != "df -h" {
		t.Errorf("confirmations = %+v, %v", confs, err)
	}
	if sessions, err := store.ListSessions(ctx); err != nil || len(sessions) != 1 {
		t.Errorf("sessions = %+v, %v", sessions, err)
	}

	// And it keeps working: a new row lands beside the old ones.
	if err := store.CreateMessage(ctx, &registry.Message{ID: "m-new", ConversationID: "conv-1",
		Role: registry.MessageRoleUser, Content: "after the upgrade"}); err != nil {
		t.Errorf("writing after the upgrade: %v", err)
	}
}

// TestUpgradeFromV020: production runs v0.2.0, so its database must open
// with today's code too: every migration since applied (the audit trail,
// SSH port and host keys, the relay policy), the vault still decrypting,
// and the columns added since reading back as their defaults.
func TestUpgradeFromV020(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v020.db")
	seedAt(t, dbPath, v020Schema, v020Seed)

	store, err := Open(dbPath, WithMasterKey(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatalf("Open (applies every migration after v0.2.0): %v", err)
	}
	defer store.Close()

	creds, err := store.ListCredentials(ctx)
	if err != nil || len(creds) != 2 {
		t.Fatalf("credentials = %+v, %v; want both", creds, err)
	}
	for _, c := range creds {
		got, err := store.GetCredential(ctx, c.ID)
		if err != nil || got.Value != "upgrade-test-value" {
			t.Errorf("credential %s = %+v, %v; want it decrypting as before", c.ID, got, err)
		}
	}

	// Columns since v0.2.0 read back as their defaults: no SSH override
	// or pin (LOOM-114), the purpose's relay default (work: none).
	jet, err := store.GetTarget(ctx, "t-jet")
	if err != nil || jet.SSHPort != 0 || jet.HostKeys != "" || jet.Policy.Relay != "" ||
		jet.Policy.EffectiveRelay() != registry.RelayFull {
		t.Errorf("target jet01 = %+v, %v; want no SSH override or pin, relay full by default", jet, err)
	}
	sc1, err := store.GetTarget(ctx, "t-work")
	if err != nil || sc1.Policy.EffectiveRelay() != registry.RelayNone || !sc1.Policy.RequireConfirmation {
		t.Errorf("target sc1 = %+v, %v; want its work policy kept and relay none by default", sc1, err)
	}
	msgs, err := store.ListMessagesByConversation(ctx, "conv-1")
	if err != nil || len(msgs) != 3 || msgs[1].Origin != registry.TargetPurposePersonal || msgs[1].OriginTargetID != "" {
		t.Errorf("messages = %+v, %v; want them kept, with no origin target recorded", msgs, err)
	}
	if events, err := store.ListDispatchEventsByConversation(ctx, "conv-1"); err != nil || len(events) != 0 {
		t.Errorf("dispatch events = %+v, %v; want none yet", events, err)
	}
	if d, err := store.GetDispatch(ctx, "d-2"); err != nil || d.ConfirmationID != "c-1" {
		t.Errorf("dispatch = %+v, %v", d, err)
	}

	// The new tables and columns take writes beside the old rows.
	if err := store.CreateDispatchEvent(ctx, &registry.DispatchEvent{ID: "e-new", ConversationID: "conv-1",
		DispatchID: "d-2", Kind: registry.EventOutcome}); err != nil {
		t.Errorf("writing an event after the upgrade: %v", err)
	}
	if err := store.SetTargetHostKeys(ctx, "t-jet", "jet01 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"); err != nil {
		t.Errorf("pinning after the upgrade: %v", err)
	}
	if err := store.CreateMessage(ctx, &registry.Message{ID: "m-new", ConversationID: "conv-1", OriginTargetID: "t-work",
		Role: registry.MessageRoleUser, Content: "after the upgrade"}); err != nil {
		t.Errorf("writing a message after the upgrade: %v", err)
	}
}
