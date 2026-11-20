package providers_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
)

// LOOM-272: the commands for a person who only watches. The host asks a
// plugin read-only only when its manifest declares
// targets.attach_read_only, and hands on only commands that attach
// read-only; otherwise the answer is that the plugin can't, never a
// command that can type.

// setCapabilities rewrites the installed plugin's capabilities, as a
// plugin whose manifest declares only those would have them.
func (h *harness) setCapabilities(t *testing.T, keep func(string) bool) {
	t.Helper()
	ctx := context.Background()
	rows, err := h.store.ListPlugins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		if p.ID != h.pluginID {
			continue
		}
		p.Capabilities = slices.DeleteFunc(p.Capabilities, func(c string) bool { return !keep(c) })
		if err := h.store.UpdatePlugin(ctx, p); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("plugin %s isn't installed", h.pluginID)
}

func TestReadOnlyAttachCommands(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	target, env := h.create(t, "watched", true)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	h.m.TmuxSocket = "loomux-b"

	cmds, err := h.m.ReadOnlyAttachCommands(ctx, target.ID, "loomux-x")
	if err != nil || len(cmds) != 1 || cmds[0].Via != "fake" {
		t.Fatalf("ReadOnlyAttachCommands = %+v, %v; want the fake's one command", cmds, err)
	}
	want := []string{"fake", "attach", env.ID, "--", "tmux", "-L", "loomux-b", "attach", "-r", "-t", "loomux-x"}
	if !slices.Equal(cmds[0].Argv, want) || cmds[0].Command != strings.Join(want, " ") {
		t.Errorf("read-only command = %q (argv %q); want %q on the instance's socket", cmds[0].Command, cmds[0].Argv, want)
	}
	// The writable ones are unchanged.
	writable, err := h.m.AttachCommands(ctx, target.ID, "loomux-x")
	if err != nil || len(writable) != 1 || slices.Contains(writable[0].Argv, "-r") {
		t.Errorf("AttachCommands = %+v, %v; want the writable command", writable, err)
	}

	// A target no plugin made has no plugin command of either kind.
	plain := &registry.Target{ID: "plain-1", Name: "jet01", Kind: registry.TargetKindRemote, Host: "jet01.example.com", User: "orski"}
	if err := h.store.CreateTarget(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if cmds, err := h.m.ReadOnlyAttachCommands(ctx, plain.ID, "loomux-x"); !errors.Is(err, providers.ErrNotMachine) || len(cmds) != 0 {
		t.Errorf("ReadOnlyAttachCommands for a plain target = %+v, %v; want ErrNotMachine", cmds, err)
	}

	// A plugin that doesn't declare the capability isn't asked: it would
	// answer a request it doesn't understand with its writable command
	// (here it even could answer read-only; the manifest decides).
	h.setCapabilities(t, func(c string) bool { return c != protocol.CapTargetsAttachReadOnly })
	cmds, err = h.m.ReadOnlyAttachCommands(ctx, target.ID, "loomux-x")
	var unsupported *providers.WatchUnsupportedError
	if !errors.As(err, &unsupported) || len(cmds) != 0 {
		t.Fatalf("ReadOnlyAttachCommands without the capability = %+v, %v; want a WatchUnsupportedError and no command", cmds, err)
	}
	if unsupported.Plugin != "fake" || !strings.Contains(unsupported.Error(), "fake") {
		t.Errorf("WatchUnsupportedError = %+v (%v); want it to name the plugin", unsupported, unsupported)
	}
	if writable, err := h.m.AttachCommands(ctx, target.ID, "loomux-x"); err != nil || len(writable) != 1 {
		t.Errorf("AttachCommands without the read-only capability = %+v, %v; the writable command is unaffected", writable, err)
	}

	// A plugin with no attach command of its own at all has none to
	// refuse: the plain ssh form is all there is, for a watch too.
	h.setCapabilities(t, func(c string) bool {
		return c != protocol.CapTargetsAttachReadOnly && c != protocol.CapTargetsAttachCommands
	})
	if cmds, err := h.m.ReadOnlyAttachCommands(ctx, target.ID, "loomux-x"); err != nil || len(cmds) != 0 {
		t.Errorf("ReadOnlyAttachCommands for a plugin with no attach commands = %+v, %v; want none and no error", cmds, err)
	}
}

// A plugin that declares the capability and confirms read_only, but
// answers with a command that can type, is not believed: the command is
// dropped, and with nothing left the plugin can't serve a watch.
func TestReadOnlyAttachCommands_WritableAnswerIsDropped(t *testing.T) {
	h := newHarness(t, map[string]any{"targets_mode": fake.TargetsWatchWritable})
	ctx := context.Background()
	target, _ := h.create(t, "watched", true)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)

	cmds, err := h.m.ReadOnlyAttachCommands(ctx, target.ID, "loomux-x")
	var unsupported *providers.WatchUnsupportedError
	if !errors.As(err, &unsupported) || len(cmds) != 0 {
		t.Fatalf("ReadOnlyAttachCommands from a plugin answering writable = %+v, %v; want a WatchUnsupportedError and no command", cmds, err)
	}
}
