package kubernetes

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

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
	p := configured(t, fake.NewClientset(), nil)
	env, err := p.CreateTarget(ctx, spec("abc123"))
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
		if want := " -- " + targets.AttachCommand(exec.TmuxSocket(), "loomux-x"); len(cmds) != 1 || !strings.HasSuffix(cmds[0].Command, want) {
			t.Errorf("instance socket %q: attach = %+v, want a command ending %q", instance, cmds, want)
		}
		// LOOM-269: the same command as arguments to execute with no
		// shell, through the SDK's JSON as the host receives it.
		argv := cmds[0].Argv
		if n := len(argv); n != 13 || argv[0] != "kubectl" || argv[1] != "-n" || argv[3] != "exec" || argv[4] != "-it" || argv[6] != "--" ||
			!slices.Equal(argv[7:], []string{"tmux", "-L", exec.TmuxSocket(), "attach", "-t", "loomux-x"}) || strings.Join(argv, " ") != cmds[0].Command {
			t.Errorf("instance socket %q: argv = %q, command %q", instance, argv, cmds[0].Command)
		}
	}

	params, _ := json.Marshal(protocol.AttachParams{ID: env.ID, Session: "loomux-x", Socket: "b; rm -rf /"})
	if _, rerr := handle(ctx, protocol.MethodTargetsAttachCommands, params); rerr == nil || rerr.Code != rpc.CodeInvalidParams {
		t.Errorf("a bad socket through the SDK: %v", rerr)
	}
	if _, err := p.TargetAttachCommands(ctx, env.ID, "loomux-x", "b; rm -rf /"); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("a bad socket: %v", err)
	}
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
	p := configured(t, fake.NewClientset(), nil)
	env, err := p.CreateTarget(ctx, spec("abc123"))
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

	watch := ask(protocol.AttachParams{ID: env.ID, Session: "loomux-x", Socket: "loomux-b", ReadOnly: true})
	want := []string{"kubectl", "-n", "loomux-agents", "exec", "-it", "lx-abc123", "--", "tmux", "-L", "loomux-b", "attach", "-r", "-t", "loomux-x"}
	if !watch.ReadOnly || len(watch.Commands) != 1 || watch.Commands[0].Via != "kubectl" || !slices.Equal(watch.Commands[0].Argv, want) ||
		watch.Commands[0].Command != strings.Join(want, " ") {
		t.Fatalf("read-only attach = %+v; want %q, confirmed read_only", watch, want)
	}
	if !protocol.AttachesReadOnly(watch.Commands[0].Argv) {
		t.Errorf("the host wouldn't take %q as read-only", watch.Commands[0].Argv)
	}

	plain := ask(protocol.AttachParams{ID: env.ID, Session: "loomux-x", Socket: "loomux-b"})
	if plain.ReadOnly || len(plain.Commands) != 1 || slices.Contains(plain.Commands[0].Argv, "-r") {
		t.Errorf("a plain request = %+v; want the writable command, read_only unset", plain)
	}

	// The same grammar and the same lookups as the writable form.
	if _, err := p.TargetAttachCommandsReadOnly(ctx, env.ID, "x; rm -rf /", "loomux"); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("read-only attach with a bad session: %v", err)
	}
	if _, err := p.TargetAttachCommandsReadOnly(ctx, env.ID, "loomux-x", "b; rm -rf /"); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("read-only attach with a bad socket: %v", err)
	}
	if _, err := p.TargetAttachCommandsReadOnly(ctx, "nope", "loomux-x", "loomux"); rpcCode(err) != rpc.CodeNotFound {
		t.Errorf("read-only attach to an unknown machine: %v", err)
	}
}
