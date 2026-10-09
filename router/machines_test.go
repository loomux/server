package router_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
)

// LOOM-178: machines plugins made, as the router sees them.

type machineStub struct {
	mu      sync.Mutex
	status  string
	startEr error
	started []string
}

func (m *machineStub) info(ctx context.Context, targetID string) (string, bool, string, bool) {
	return "kubernetes", true, m.status, true
}

func (m *machineStub) start(ctx context.Context, targetID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, targetID)
	return m.startEr
}

func TestMachines_SnapshotCarriesTheMachine(t *testing.T) {
	store := newTestStore(t)
	stub := &machineStub{status: "stopped"}
	_, r, model := newRouter(t, store, router.WithMachines(stub.info, stub.start))
	target := &registry.Target{ID: uuid.NewString(), Name: "builds", Kind: registry.TargetKindRemote, Host: "lx-a.fake.invalid", User: "agent"}
	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-m", "hello"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(model.LastDecideTargets) != 1 {
		t.Fatalf("targets = %+v", model.LastDecideTargets)
	}
	got := model.LastDecideTargets[0]
	if got.CreatedBy != "kubernetes" || !got.Ephemeral || got.MachineStatus != "stopped" {
		t.Errorf("snapshot = %+v", got)
	}
}

func TestMachines_StartedBeforeDispatchAndRefusedWhenItCant(t *testing.T) {
	store := newTestStore(t)
	stub := &machineStub{status: "stopped", startEr: errors.New("the machine is lost")}
	_, r, model := newRouter(t, store, router.WithMachines(stub.info, stub.start))
	ctx := context.Background()
	target := &registry.Target{ID: uuid.NewString(), Name: "builds", Kind: registry.TargetKindRemote, Host: "lx-a.fake.invalid", User: "agent"}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "proj", Path: "/data/work/proj", TargetID: target.ID, Status: registry.WorkspaceStatusIdle}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	_, err := r.Dispatch(ctx, "conv-m2", "do it")
	if err == nil || !strings.Contains(err.Error(), "the machine is lost") {
		t.Fatalf("dispatch to a machine that can't start: want its reason, got %v", err)
	}
	stub.mu.Lock()
	started := append([]string{}, stub.started...)
	stub.mu.Unlock()
	if len(started) == 0 || started[0] != target.ID {
		t.Errorf("the starter wasn't asked for the target: %v", started)
	}
	if len(store_tasks(t, store)) != 0 {
		t.Error("a task was created for a machine that never started")
	}
}

func store_tasks(t *testing.T, store registry.Store) []*registry.Task {
	t.Helper()
	tasks, err := store.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return tasks
}
