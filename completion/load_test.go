package completion_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/internal/tmuxtest"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/sshtest"
)

// TestLoad_TargetOpLatency measures target-op latency while 1, 5 and 10
// marker-tier turns wait on one remote target whose every ssh operation
// costs loadLatency, the SOCKS path to jet01 (LOOM-254, LOOM-234 F7). It
// runs the real Detector at its production intervals, plus a foreground
// HasSession every second as the router and API would make. Opt-in, as it
// takes a while: LOOMUX_LOAD=1 (LOOMUX_LOAD_SECONDS per scenario, default
// 30; LOOMUX_LOAD_SESSIONS, default "1,5,10").
func TestLoad_TargetOpLatency(t *testing.T) {
	if os.Getenv("LOOMUX_LOAD") == "" {
		t.Skip("set LOOMUX_LOAD=1 to run the target-op load test")
	}
	run := 30 * time.Second
	if s, err := strconv.Atoi(os.Getenv("LOOMUX_LOAD_SECONDS")); err == nil && s > 0 {
		run = time.Duration(s) * time.Second
	}
	var report strings.Builder
	fmt.Fprintf(&report, "\n%-8s %-16s %6s %8s %8s %8s %6s\n", "sessions", "op", "count", "p50", "p95", "max", "ops/s")
	sessions := []int{1, 5, 10}
	if list := os.Getenv("LOOMUX_LOAD_SESSIONS"); list != "" {
		sessions = nil
		for _, f := range strings.Split(list, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 {
				t.Fatalf("LOOMUX_LOAD_SESSIONS %q: want a comma-separated list of session counts", list)
			}
			sessions = append(sessions, n)
		}
	}
	for _, n := range sessions {
		rec := measureLoad(t, n, run)
		for _, op := range rec.ops() {
			ds := rec.samples[op]
			fmt.Fprintf(&report, "%-8d %-16s %6d %8s %8s %8s %6.1f\n", n, op, len(ds),
				quantile(ds, 0.50), quantile(ds, 0.95), quantile(ds, 1), float64(len(ds))/run.Seconds())
		}
		for op, err := range rec.errs {
			fmt.Fprintf(&report, "%-8d %-16s last error: %s\n", n, op, err)
		}
	}
	t.Log(report.String())
}

const (
	loadLatency = 300 * time.Millisecond
	loadWarmup  = 3 * time.Second
)

func measureLoad(t *testing.T, sessions int, run time.Duration) *opRecorder {
	t.Helper()
	server := sshtest.Start(t)
	server.SetLatency(loadLatency)
	socket := tmuxtest.Socket(t)
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindRemote)

	rec := &opRecorder{samples: map[string][]time.Duration{}}
	newRemote := func() targets.TargetExecutor {
		return targets.NewRemoteExecutor(server.Host, "loomux-test",
			targets.WithTmuxSocket(socket),
			targets.WithPort(server.Port),
			targets.WithIdentityFile(server.IdentityFile),
			targets.WithExtraSSHArgs("-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentitiesOnly=yes"),
		)
	}
	t.Cleanup(func() { _ = newRemote().Close() })
	factory := func(*registry.Target) (targets.TargetExecutor, error) {
		return newRecordingExecutor(newRemote(), rec, ""), nil
	}

	// The panes change every second, as an agent's status line does
	// while it works.
	local := targets.NewLocalExecutor(socket)
	var tasks []*registry.Task
	for i := 0; i < sessions; i++ {
		session := fmt.Sprintf("loomux-load-%d-%s", i, uuid.NewString()[:8])
		if err := local.NewSession(context.Background(), session, t.TempDir(), "while :; do date +%s; sleep 1; done"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		tasks = append(tasks, &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, AgentType: "claude-code", TmuxSession: session})
	}

	cfg := completion.Config{"claude-code": {Tier: completion.TierMarker}}
	d := completion.NewDetector(store, factory, cfg, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), loadWarmup+run)
	defer cancel()
	rec.from = time.Now().Add(loadWarmup)
	rec.until, _ = ctx.Deadline()

	// Turns start at different times in production: spread them over
	// one poll interval rather than lining every poll up.
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * time.Second / time.Duration(sessions))
			if err := d.Wait(ctx, task); ctx.Err() == nil {
				t.Errorf("Wait(%s) ended early: %v", task.TmuxSession, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		fg := newRecordingExecutor(newRemote(), rec, "fg_")
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if _, err := fg.HasSession(ctx, tasks[0].TmuxSession); err != nil && ctx.Err() == nil {
				t.Errorf("HasSession: %v", err)
			}
		}
	}()
	wg.Wait()
	return rec
}

// opRecorder keeps every operation's latency, by operation, from from
// until until.
type opRecorder struct {
	mu      sync.Mutex
	from    time.Time
	until   time.Time
	samples map[string][]time.Duration
	errs    map[string]string // the last error, by operation
}

func (r *opRecorder) add(op string, start time.Time, err error) {
	if err != nil && strings.Contains(err.Error(), "context canceled") {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Operations the run's end cut short aren't latencies.
	if start.Before(r.from) || !time.Now().Before(r.until) {
		return
	}
	if err != nil {
		op += "_err"
		if r.errs == nil {
			r.errs = map[string]string{}
		}
		r.errs[op] = err.Error()
	}
	r.samples[op] = append(r.samples[op], time.Since(start))
}

func (r *opRecorder) ops() []string {
	var ops []string
	for op := range r.samples {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	return ops
}

func quantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(math.Ceil(q*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i].Round(time.Millisecond)
}

// recordingExecutor times the operations completion polls with.
type recordingExecutor struct {
	targets.TargetExecutor
	rec    *opRecorder
	prefix string
}

func newRecordingExecutor(inner targets.TargetExecutor, rec *opRecorder, prefix string) targets.TargetExecutor {
	return &recordingExecutor{TargetExecutor: inner, rec: rec, prefix: prefix}
}

func (e *recordingExecutor) FileExists(ctx context.Context, path string) (bool, error) {
	start := time.Now()
	ok, err := e.TargetExecutor.FileExists(ctx, path)
	e.rec.add(e.prefix+"file_exists", start, err)
	return ok, err
}

func (e *recordingExecutor) PaneExited(ctx context.Context, target string) (*targets.PaneExit, error) {
	start := time.Now()
	exit, err := e.TargetExecutor.PaneExited(ctx, target)
	e.rec.add(e.prefix+"pane_exited", start, err)
	return exit, err
}

func (e *recordingExecutor) CapturePane(ctx context.Context, target string) (string, error) {
	start := time.Now()
	out, err := e.TargetExecutor.CapturePane(ctx, target)
	e.rec.add(e.prefix+"capture_pane", start, err)
	return out, err
}

func (e *recordingExecutor) HasSession(ctx context.Context, session string) (bool, error) {
	start := time.Now()
	ok, err := e.TargetExecutor.HasSession(ctx, session)
	e.rec.add(e.prefix+"has_session", start, err)
	return ok, err
}

func (e *recordingExecutor) Poll(ctx context.Context, req targets.PollRequest) (*targets.PollResult, error) {
	start := time.Now()
	res, err := e.TargetExecutor.(targets.Poller).Poll(ctx, req)
	e.rec.add(e.prefix+"poll", start, err)
	return res, err
}
