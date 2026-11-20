package docker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/plugins/sdk"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// LOOM-210: the attach command lands on the tmux server the machine's
// sessions really run on. The image starts no tmux of its own: the
// host runs every session over ssh with the executor it makes for the
// machine's target, on its instance's socket, and sends that socket in
// targets.attach_commands. So the command, asked for through the SDK
// as the host asks, ends in the host's own attach for that executor.
// A host from before LOOM-210 sends none: the host's default.
func TestAttachCommandUsesTheSessionsSocket(t *testing.T) {
	p, _ := fakePlugin(t, nil)
	env, err := p.CreateTarget(ctx, spec("k7f3q2"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	target := &registry.Target{Kind: registry.TargetKindRemote, Host: env.Address.Host, SSHPort: env.Address.Port, User: "agent"}
	handle := sdk.Handler(p, nil)
	for _, instance := range []string{"loomux-b", ""} {
		exec, err := targets.NewExecutorWith(target, targets.ExecutorOptions{TmuxSocket: instance, KnownHostsDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		cmds := attachVia(t, handle, protocol.AttachParams{ID: env.ID, Session: "loomux-x", Socket: instance})
		if want := " " + targets.AttachCommand(exec.TmuxSocket(), "loomux-x"); len(cmds) != 1 || !strings.HasSuffix(cmds[0].Command, want) {
			t.Errorf("instance socket %q: attach = %+v, want a command ending %q", instance, cmds, want)
		}
	}

	params, _ := json.Marshal(protocol.AttachParams{ID: env.ID, Session: "loomux-x", Socket: "b; rm -rf /"})
	if _, rerr := handle(ctx, protocol.MethodTargetsAttachCommands, params); rerr == nil || rerr.Code != rpc.CodeInvalidParams {
		t.Errorf("a bad socket through the SDK: %v", rerr)
	}
	_, err = p.TargetAttachCommands(ctx, env.ID, "loomux-x", "b; rm -rf /")
	wantCode(t, err, rpc.CodeInvalidParams)
}

// attachVia calls targets.attach_commands through handle.
func attachVia(t *testing.T, handle rpc.Handler, p protocol.AttachParams) []protocol.AttachCommand {
	t.Helper()
	params, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	res, rerr := handle(ctx, protocol.MethodTargetsAttachCommands, params)
	if rerr != nil {
		t.Fatalf("targets.attach_commands: %v", rerr)
	}
	out, _ := json.Marshal(res)
	var att protocol.AttachResult
	if err := json.Unmarshal(out, &att); err != nil {
		t.Fatal(err)
	}
	return att.Commands
}

// LOOM-272: asked read-only, through the SDK as the host asks, the
// command attaches with tmux's -r, so a person who only watches can't
// type into the pane, and the answer says it is read-only. The manifest
// declares the capability the host asks under. A plain request still
// gets the writable command, unconfirmed.
func TestAttachCommandReadOnly(t *testing.T) {
	p, _ := fakePlugin(t, nil)
	env, err := p.CreateTarget(ctx, spec("k7f3q2"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m, err := p.Describe(ctx); err != nil || !m.HasCapability(protocol.CapTargetsAttachReadOnly) || !m.HasCapability(protocol.CapTargetsAttachCommands) {
		t.Fatalf("manifest = %+v, %v; want it to declare %s", m, err, protocol.CapTargetsAttachReadOnly)
	}
	handle := sdk.Handler(p, nil)
	ask := func(p protocol.AttachParams) protocol.AttachResult {
		t.Helper()
		params, _ := json.Marshal(p)
		res, rerr := handle(ctx, protocol.MethodTargetsAttachCommands, params)
		if rerr != nil {
			t.Fatalf("targets.attach_commands %+v: %v", p, rerr)
		}
		out, _ := json.Marshal(res)
		var att protocol.AttachResult
		if err := json.Unmarshal(out, &att); err != nil {
			t.Fatal(err)
		}
		return att
	}

	plain := ask(protocol.AttachParams{ID: env.ID, Session: "loomux-x", Socket: "loomux-b"})
	if plain.ReadOnly || len(plain.Commands) != 1 || !strings.HasSuffix(plain.Commands[0].Command, " tmux -L loomux-b attach -t loomux-x") {
		t.Fatalf("a plain request = %+v; want the writable command, read_only unset", plain)
	}
	watch := ask(protocol.AttachParams{ID: env.ID, Session: "loomux-x", Socket: "loomux-b", ReadOnly: true})
	if !watch.ReadOnly || len(watch.Commands) != 1 || watch.Commands[0].Via != "docker" {
		t.Fatalf("read-only attach = %+v; want one docker command, confirmed read_only", watch)
	}
	// The writable command with -r after attach, and nothing else changed.
	got, writable := watch.Commands[0], plain.Commands[0]
	if want := strings.Replace(writable.Command, " attach -t ", " attach -r -t ", 1); got.Command != want || strings.Join(got.Argv, " ") != got.Command ||
		len(got.Argv) != len(writable.Argv)+1 {
		t.Errorf("read-only attach = %q (argv %q); want %q", got.Command, got.Argv, want)
	}
	if !protocol.AttachesReadOnly(got.Argv) {
		t.Errorf("the host wouldn't take %q as read-only", got.Argv)
	}

	// The same grammar and the same lookups as the writable form.
	_, err = p.TargetAttachCommandsReadOnly(ctx, env.ID, "x; rm -rf /", "loomux")
	wantCode(t, err, rpc.CodeInvalidParams)
	_, err = p.TargetAttachCommandsReadOnly(ctx, env.ID, "loomux-x", "b; rm -rf /")
	wantCode(t, err, rpc.CodeInvalidParams)
	_, err = p.TargetAttachCommandsReadOnly(ctx, "nope00", "loomux-x", "loomux")
	wantCode(t, err, rpc.CodeNotFound)
}
