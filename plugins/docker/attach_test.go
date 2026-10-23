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
