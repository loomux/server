package providers_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/registry"
)

// pullHarness is one over the fake in mode (with its extra config).
// The deadlines are the test's to set: the first poll that sees a pull
// comes a create call and a store write after the job starts, which a
// loaded CI runner can stretch past a few hundred milliseconds.
func pullHarness(t *testing.T, mode string, createTimeout, pullTimeout time.Duration, config map[string]any) *harness {
	t.Helper()
	cfg := map[string]any{"targets_mode": mode}
	for k, v := range config {
		cfg[k] = v
	}
	h := newHarness(t, cfg)
	h.m.CreateTimeout = createTimeout
	h.m.PullTimeout = pullTimeout
	return h
}

// pullingSoon waits up to 30 s for the machine to show its pull,
// failing at once if it ends in error first.
func pullingSoon(t *testing.T, h *harness, targetID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		env, err := h.m.Get(t.Context(), targetID)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case env.Status == registry.EnvironmentStarting && env.StatusReason == "pulling the image":
			return
		case env.Status == registry.EnvironmentError:
			t.Fatalf("error before the pull was seen: %s", env.StatusReason)
		case time.Now().After(deadline):
			t.Fatalf("never shown pulling: %s (%s)", env.Status, env.StatusReason)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// LOOM-203: a machine whose image is still pulling past CreateTimeout
// isn't failed: the deadline stands still while the plugin reports the
// pull in progress, and the machine runs once it ends. The test, not
// the clock, ends the pull (the fake's pull_gate).
func TestSlowPullIsNotACreateFailure(t *testing.T) {
	const createTimeout = 3 * time.Second
	gate := filepath.Join(t.TempDir(), "pulled")
	h := pullHarness(t, fake.TargetsSlowPull, createTimeout, time.Minute, map[string]any{"pull_gate": gate})
	target, _ := h.create(t, "slow-pull", true)
	pullingSoon(t, h, target.ID)
	time.Sleep(createTimeout + time.Second) // the pull outlasts CreateTimeout
	env, err := h.m.Get(t.Context(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if env.Status != registry.EnvironmentStarting {
		t.Fatalf("pulling past CreateTimeout: %s (%s), want starting", env.Status, env.StatusReason)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
}

// While it pulls, the plugin's words are the machine's reason.
func TestPullReasonShows(t *testing.T) {
	h := pullHarness(t, fake.TargetsPullForever, time.Hour, time.Hour, nil)
	target, _ := h.create(t, "pulling", true)
	pullingSoon(t, h, target.ID)
}

// With no progress reported, the deadline applies as before.
func TestStuckMachineStillFailsAtTheDeadline(t *testing.T) {
	h := pullHarness(t, fake.TargetsStuck, 300*time.Millisecond, 10*time.Second, nil)
	target, _ := h.create(t, "stuck", true)
	env := h.waitStatus(t, target.ID, registry.EnvironmentError)
	if env.StatusReason != "the machine didn't become ready within 300ms" {
		t.Errorf("reason = %q, want the deadline, with no pull in it", env.StatusReason)
	}
}

// A pull that never ends fails once it has used PullTimeout too, saying
// so.
func TestEndlessPullFailsAfterPullTimeout(t *testing.T) {
	const createTimeout, pullTimeout = 3 * time.Second, time.Second
	h := pullHarness(t, fake.TargetsPullForever, createTimeout, pullTimeout, nil)
	start := time.Now()
	target, _ := h.create(t, "pull-forever", true)
	pullingSoon(t, h, target.ID)
	env := h.waitStatus(t, target.ID, registry.EnvironmentError)
	if took := time.Since(start); took < createTimeout+pullTimeout-100*time.Millisecond {
		t.Errorf("failed after %s, before CreateTimeout plus PullTimeout", took)
	}
	if !strings.Contains(env.StatusReason, "didn't become ready within 3s, besides 1s of pulling its image, which hadn't finished") {
		t.Errorf("reason = %q", env.StatusReason)
	}
}
