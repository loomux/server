package completion_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// pollingExecutor is a target that answers batched polls (LOOM-254). It
// counts them and the files and panes each asked about, and counts any
// FileExists or PaneExited, which a batched wait should only make for a
// pane the poll didn't see running.
type pollingExecutor struct {
	scriptedExecutor
	mu          sync.Mutex
	markers     map[string]bool // marker paths that exist
	dead        map[string]bool // panes whose process exited
	pollErr     error
	polls       int
	maxFiles    int
	maxPanes    int
	fileExists  int
	paneExiteds int
}

func newPollingExecutor() *pollingExecutor {
	return &pollingExecutor{scriptedExecutor: *newScriptedExecutor([]string{""}), markers: map[string]bool{}, dead: map[string]bool{}}
}

func (e *pollingExecutor) Poll(_ context.Context, req targets.PollRequest) (*targets.PollResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.polls++
	e.maxFiles = max(e.maxFiles, len(req.Files))
	e.maxPanes = max(e.maxPanes, len(req.Panes))
	if e.pollErr != nil {
		return nil, e.pollErr
	}
	res := &targets.PollResult{Exists: make([]bool, len(req.Files)), Running: make([]bool, len(req.Panes))}
	for i, f := range req.Files {
		res.Exists[i] = e.markers[f]
	}
	for i, p := range req.Panes {
		res.Running[i] = !e.dead[p]
	}
	return res, nil
}

func (e *pollingExecutor) FileExists(_ context.Context, path string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fileExists++
	return e.markers[path], nil
}

func (e *pollingExecutor) PaneExited(_ context.Context, pane string) (*targets.PaneExit, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.paneExiteds++
	if e.dead[pane] {
		return &targets.PaneExit{Status: 3, Output: "boom"}, nil
	}
	return nil, nil
}

func (e *pollingExecutor) snapshot() pollingExecutor {
	e.mu.Lock()
	defer e.mu.Unlock()
	return pollingExecutor{polls: e.polls, maxFiles: e.maxFiles, maxPanes: e.maxPanes, fileExists: e.fileExists, paneExiteds: e.paneExiteds}
}

func newPollingDetector(t *testing.T, exec *pollingExecutor, markerDir string, opts ...completion.DetectorOption) (*completion.Detector, *registry.Workspace) {
	t.Helper()
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindRemote)
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	cfg := completion.Config{"agent": {Tier: completion.TierMarker, NoProgressTimeout: -1}}
	opts = append([]completion.DetectorOption{completion.WithPollInterval(20 * time.Millisecond)}, opts...)
	return completion.NewDetector(store, factory, cfg, markerDir, opts...), ws
}

// Ten turns waiting on one target share its polls (LOOM-254): every poll
// asks about all ten markers and panes at once, no turn makes calls of
// its own, every turn still ends at its marker, and the polling stops
// with the last wait.
func TestDetector_BatchesPollsPerTarget(t *testing.T) {
	exec := newPollingExecutor()
	markerDir := t.TempDir()
	d, ws := newPollingDetector(t, exec, markerDir)

	const turns = 10
	errs := make(chan error, turns)
	var tasks []*registry.Task
	for i := 0; i < turns; i++ {
		task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s-" + uuid.NewString()}
		tasks = append(tasks, task)
		go func() { errs <- d.Wait(context.Background(), task) }()
	}
	waitFor(t, func() bool { s := exec.snapshot(); return s.maxFiles == turns && s.maxPanes == turns })
	time.Sleep(200 * time.Millisecond) // ~10 polling intervals

	exec.mu.Lock()
	for _, task := range tasks {
		exec.markers[completion.MarkerPath(markerDir, task.ID)] = true
	}
	exec.mu.Unlock()
	for range tasks {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("Wait = %v, want nil at the marker", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a wait never saw its marker")
		}
	}

	s := exec.snapshot()
	if s.fileExists != 0 || s.paneExiteds != 0 {
		t.Errorf("FileExists %d, PaneExited %d times, want neither: every check batched", s.fileExists, s.paneExiteds)
	}
	// Unbatched, 10 turns × 2 checks × ~10+ intervals would be 200+.
	if s.polls > 60 {
		t.Errorf("%d polls, want about one per interval", s.polls)
	}
	time.Sleep(100 * time.Millisecond)
	if after := exec.snapshot().polls; after != s.polls {
		t.Errorf("%d polls after the last wait ended, want none", after-s.polls)
	}
}

// A pane the poll doesn't see running is asked about with PaneExited,
// which says how it ended: a marker-tier turn ends as ProcessExitedError,
// a command task (TierExit) as done.
func TestDetector_BatchedPollAsksPaneExitedForADeadPane(t *testing.T) {
	exec := newPollingExecutor()
	d, ws := newPollingDetector(t, exec, t.TempDir())
	exec.dead["s-dead"] = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agent := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s-dead"}
	var exited *orchestrator.ProcessExitedError
	if err := d.Wait(ctx, agent); !errors.As(err, &exited) || exited.Status != 3 {
		t.Errorf("agent Wait = %v, want ProcessExitedError with status 3", err)
	}
	command := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindCommand, TmuxSession: "s-dead"}
	if err := d.Wait(ctx, command); err != nil {
		t.Errorf("command Wait = %v, want nil once its pane exited", err)
	}
	if s := exec.snapshot(); s.fileExists != 0 {
		t.Errorf("FileExists called %d times, want 0", s.fileExists)
	}
}

// A batched poll's failure reaches every wait as its own call's would:
// an unreachable target is ridden out for the grace, then fails the turn.
func TestDetector_BatchedPollUnreachable(t *testing.T) {
	exec := newPollingExecutor()
	exec.pollErr = &targets.UnreachableError{Host: "jet01", Failure: targets.SSHTimeout}
	d, ws := newPollingDetector(t, exec, t.TempDir(), completion.WithUnreachableGrace(100*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
	start := time.Now()
	err := d.Wait(ctx, task)
	if !errors.Is(err, targets.ErrUnreachable) {
		t.Errorf("Wait = %v, want ErrUnreachable after the grace", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("Wait failed after %s, want the 100ms grace ridden out first", elapsed)
	}
}

// LOOM-254: the no-progress check backs off while the pane doesn't
// change, and doesn't while it does.
func TestDetector_ProgressChecksBackOffWhileUnchanged(t *testing.T) {
	captures := func(busy bool) int {
		store := newTestStore(t)
		ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
		task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "agent", TmuxSession: "s"}
		exec := &countingCaptures{stuckExecutor: stuckExecutor{scriptedExecutor: *newScriptedExecutor([]string{""}), busy: busy}}
		factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
		d := completion.NewDetector(store, factory, completion.Config{"agent": {
			Tier: completion.TierMarker, MaxTurnDuration: 600 * time.Millisecond, NoProgressTimeout: time.Hour,
		}}, t.TempDir(), completion.WithProgressPollInterval(20*time.Millisecond))
		_ = d.Wait(context.Background(), task)
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return exec.n
	}
	busy, idle := captures(true), captures(false)
	// Busy: every 20ms, ~30. Unchanged: 20, 40, then every 60ms, ~11.
	if idle*2 > busy {
		t.Errorf("%d captures of an unchanged pane vs %d of a busy one, want under half", idle, busy)
	}
	if idle < 5 {
		t.Errorf("%d captures of an unchanged pane in 600ms, want the backoff capped at 3 intervals", idle)
	}
}

type countingCaptures struct {
	stuckExecutor
	mu sync.Mutex
	n  int
}

func (e *countingCaptures) CapturePane(ctx context.Context, target string) (string, error) {
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	return e.stuckExecutor.CapturePane(ctx, target)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
