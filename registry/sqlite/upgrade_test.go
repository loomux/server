package sqlite

import (
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

// TestUpgradeFromV010 (LOOM-126): a database as v0.1.0 left it opens
// with today's code, every migration since applied, and everything in it
// reads back through today's store. A migration that breaks a deployed
// database fails here, not in production. When a release changes how
// existing rows are read, extend the checks; the seed stays as v0.1.0
// wrote it.
func TestUpgradeFromV010(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v010.db")

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
	if err := goose.UpTo(db, "migrations", v010Schema); err != nil {
		t.Fatalf("goose.UpTo(%d): %v", v010Schema, err)
	}
	if _, err := db.ExecContext(ctx, v010Seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

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
