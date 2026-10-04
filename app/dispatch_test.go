package app

import (
	"context"
	"testing"
	"time"

	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

// LOOM-80: a job submitted through App runs through the real router, and
// the conversation's history holds the user message once, then the reply,
// both tied to the job.
func TestDispatches_JobRunsThroughRouter(t *testing.T) {
	srv := fakeRouterServer(t)
	app, err := Build(testConfig(t, srv.URL))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	d, err := app.Dispatches().Submit(ctx, dispatch.Request{ConversationID: "conv-1", Message: "hi"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done, err := app.Dispatches().Wait(waitCtx, d.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if done.Status != registry.DispatchStatusSucceeded || done.Reply != "hello from app" {
		t.Fatalf("dispatch = %+v", done)
	}
	msgs, err := app.Store().ListMessagesByConversation(ctx, "conv-1")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != registry.MessageRoleUser || msgs[1].Content != "hello from app" ||
		msgs[0].DispatchID != d.ID || msgs[1].DispatchID != d.ID {
		t.Fatalf("messages = %+v", msgs)
	}
}

// LOOM-80: Build marks jobs a previous process left in flight as
// interrupted, so none claims to be running with nobody running it.
func TestBuild_RecoversLeftoverDispatches(t *testing.T) {
	srv := fakeRouterServer(t)
	cfg := testConfig(t, srv.URL)
	store, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := store.CreateDispatch(context.Background(), &registry.Dispatch{
		ID: "left", ConversationID: "c", Message: "m", RequestHash: "h", Status: registry.DispatchStatusRunning,
	}, nil); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	_ = store.Close()

	app, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer app.Close()
	got, err := app.Store().GetDispatch(context.Background(), "left")
	if err != nil || got.Status != registry.DispatchStatusInterrupted {
		t.Fatalf("leftover dispatch after Build = %+v, %v", got, err)
	}
}

// DrainDispatches stops new submits.
func TestDrainDispatches_StopsSubmits(t *testing.T) {
	srv := fakeRouterServer(t)
	app, err := Build(testConfig(t, srv.URL))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer app.Close()
	if err := app.DrainDispatches(); err != nil {
		t.Fatalf("DrainDispatches: %v", err)
	}
	if _, err := app.Dispatches().Submit(context.Background(), dispatch.Request{Message: "hi"}); err == nil {
		t.Fatal("Submit after DrainDispatches succeeded")
	}
}

func TestLoadConfig_DispatchTiming(t *testing.T) {
	setRouterEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DispatchMaxDuration != 0 || cfg.DispatchDrain != defaultDispatchDrain {
		t.Errorf("defaults: max %v drain %v, want 0 (dispatch's own default) and %v", cfg.DispatchMaxDuration, cfg.DispatchDrain, defaultDispatchDrain)
	}

	t.Setenv(envDispatchMaxDuration, "3h")
	t.Setenv(envDispatchDrain, "45s")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DispatchMaxDuration != 3*time.Hour || cfg.DispatchDrain != 45*time.Second {
		t.Errorf("custom: max %v drain %v", cfg.DispatchMaxDuration, cfg.DispatchDrain)
	}

	t.Setenv(envDispatchDrain, "soon")
	if _, err := LoadConfig(); err == nil {
		t.Error("LoadConfig: want error for a malformed drain")
	}
}
