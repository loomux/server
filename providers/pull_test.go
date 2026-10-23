package providers_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/registry"
)

// pullHarness is one over the fake in mode, with a create deadline far
// shorter than the fake's pull.
func pullHarness(t *testing.T, mode string, pullTimeout time.Duration) *harness {
	t.Helper()
	h := newHarness(t, map[string]any{"targets_mode": mode})
	h.m.CreateTimeout = 300 * time.Millisecond
	h.m.PullTimeout = pullTimeout
	return h
}

// LOOM-203: a machine whose image is still pulling past CreateTimeout
// isn't failed: the deadline stands still while the plugin reports the
// pull in progress, and the machine runs once it ends.
func TestSlowPullIsNotACreateFailure(t *testing.T) {
	h := pullHarness(t, fake.TargetsSlowPull, 10*time.Second)
	target, _ := h.create(t, "slow-pull", true)
	deadline := time.Now().Add(10 * time.Second)
	for {
		env, err := h.m.Get(t.Context(), target.ID)
		if err != nil {
			t.Fatal(err)
		}
		if env.Status == registry.EnvironmentRunning {
			break
		}
		if env.Status == registry.EnvironmentError {
			t.Fatalf("the machine failed during its %s pull with a %s deadline: %s", fake.PullFor, h.m.CreateTimeout, env.StatusReason)
		}
		if time.Now().After(deadline) {
			t.Fatalf("never running: %s (%s)", env.Status, env.StatusReason)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// While it pulls, the plugin's words are the machine's reason.
func TestPullReasonShows(t *testing.T) {
	h := pullHarness(t, fake.TargetsPullForever, 10*time.Second)
	target, _ := h.create(t, "pulling", true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		env, err := h.m.Get(t.Context(), target.ID)
		if err != nil {
			t.Fatal(err)
		}
		if env.Status == registry.EnvironmentStarting && env.StatusReason == "pulling the image" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never shown pulling: %s (%s)", env.Status, env.StatusReason)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// With no progress reported, the deadline applies as before.
func TestStuckMachineStillFailsAtTheDeadline(t *testing.T) {
	h := pullHarness(t, fake.TargetsStuck, 10*time.Second)
	target, _ := h.create(t, "stuck", true)
	env := h.waitStatus(t, target.ID, registry.EnvironmentError)
	if env.StatusReason != "the machine didn't become ready within 300ms" {
		t.Errorf("reason = %q, want the deadline, with no pull in it", env.StatusReason)
	}
}

// A pull that never ends fails once it has used PullTimeout too, saying
// so.
func TestEndlessPullFailsAfterPullTimeout(t *testing.T) {
	h := pullHarness(t, fake.TargetsPullForever, 700*time.Millisecond)
	start := time.Now()
	target, _ := h.create(t, "pull-forever", true)
	env := h.waitStatus(t, target.ID, registry.EnvironmentError)
	if took := time.Since(start); took < 700*time.Millisecond {
		t.Errorf("failed after %s, before CreateTimeout plus PullTimeout", took)
	}
	if !strings.Contains(env.StatusReason, "didn't become ready within 300ms, besides 700ms of pulling its image, which hadn't finished") {
		t.Errorf("reason = %q", env.StatusReason)
	}
}
