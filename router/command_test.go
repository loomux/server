package router_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/targets"
)

// commandHarness: a router whose model always decides run_command on
// jet01 with the given command, and a fake pane that "runs" each command
// by answering from outputs (exit 0 unless listed in codes).
type commandHarness struct {
	*availabilityHarness
	outputs map[string]string
	codes   map[string]int
	relays  int
}

func newCommandHarness(t *testing.T, opts ...router.Option) *commandHarness {
	t.Helper()
	h := &commandHarness{
		availabilityHarness: newAvailabilityHarnessOpts(t, nil, opts...),
		outputs:             map[string]string{},
		codes:               map[string]int{},
	}
	h.model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		h.relays++
		return router.RelayResult{Reply: "relayed"}, nil
	}
	h.exec.paneExit = func(command string) *targets.PaneExit {
		out, ok := h.outputs[command]
		if !ok {
			return nil
		}
		return &targets.PaneExit{Status: h.codes[command], Output: out}
	}
	return h
}

func (h *commandHarness) decideCommand(command string) {
	h.decide(router.Decision{Action: router.ActionRunCommand, TargetID: h.target.ID, Command: command})
}

func (h *commandHarness) commandTasks(t *testing.T) []*registry.Task {
	t.Helper()
	all, err := h.store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	var out []*registry.Task
	for _, task := range all {
		if task.Kind == registry.TaskKindCommand {
			out = append(out, task)
		}
	}
	return out
}

// LOOM-72: a command the user wrote out in backticks runs at once on the
// target — no agent, no relay model — and its output comes back verbatim
// with its exit code, recorded on a command task.
func TestDispatch_RunCommand_VerbatimRunsAtOnce(t *testing.T) {
	h := newCommandHarness(t)
	h.decideCommand("hostname && uptime")
	h.outputs["hostname && uptime"] = "jet01\n 22:21:59 up 12 days,  load average: 0.10"

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "run `hostname && uptime` on jet01")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	for _, want := range []string{"hostname && uptime", "jet01", "exit 0", "up 12 days"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
	if h.relays != 0 {
		t.Errorf("command output went through the relay model %d time(s); it must be relayed verbatim", h.relays)
	}
	tasks := h.commandTasks(t)
	if len(tasks) != 1 || tasks[0].Command != "hostname && uptime" || tasks[0].ExitCode == nil || *tasks[0].ExitCode != 0 {
		t.Fatalf("command tasks = %+v, want one recording the command and exit 0", tasks)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 1 || cmds[0] != "hostname && uptime" {
		t.Errorf("launched %v, want exactly the user's command", cmds)
	}
}

// A command the routing model wrote itself — the user described what
// they wanted, or quoted something different — is never run without the
// user confirming the exact command; the confirmation itself never goes
// to the routing model.
func TestDispatch_RunCommand_NotVerbatim_AsksFirst(t *testing.T) {
	for _, message := range []string{
		"how long has jet01 been up?",           // no command at all
		"run hostname && uptime on jet01",       // not delimited
		"run `hostname` on jet01",               // model "completed" the command
		"run ```uptime``` and `hostname` there", // model joined two spans
	} {
		t.Run(message, func(t *testing.T) {
			h := newCommandHarness(t)
			h.decideCommand("hostname && uptime")
			h.outputs["hostname && uptime"] = "jet01"

			reply, err := h.r.Dispatch(context.Background(), "conv-1", message)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if !strings.Contains(reply, "hostname && uptime") || !strings.Contains(reply, "jet01") || !strings.Contains(reply, `"yes"`) {
				t.Errorf("reply is not a confirmation showing the exact command:\n%s", reply)
			}
			if cmds := h.exec.launchedCommands(); len(cmds) != 0 {
				t.Fatalf("ran %v before confirmation", cmds)
			}

			decides := h.decides
			reply, err = h.r.Dispatch(context.Background(), "conv-1", "yes")
			if err != nil {
				t.Fatalf("Dispatch(yes): %v", err)
			}
			if h.decides != decides {
				t.Error("the confirmation went to the routing model")
			}
			if !strings.Contains(reply, "exit 0") {
				t.Errorf("reply after confirming = %q", reply)
			}
			if len(h.commandTasks(t)) != 1 {
				t.Errorf("confirmed command did not run exactly once")
			}
		})
	}
}

func TestDispatch_RunCommand_DeclinedNeverRuns(t *testing.T) {
	h := newCommandHarness(t)
	h.decideCommand("rm -rf /tmp/scratch")
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "clean up the scratch dir on jet01"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"})
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "no, wait"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "yes"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 0 {
		t.Fatalf("a declined command ran: %v", cmds)
	}
}

// Output is bounded and never carries a credential value, and a non-zero
// exit is reported as such.
func TestDispatch_RunCommand_OutputCappedAndRedacted(t *testing.T) {
	h := newCommandHarness(t)
	if err := h.store.CreateCredential(context.Background(), &registry.Credential{
		ID: uuid.NewString(), Name: "OPENAI_API_KEY", AgentType: "codex", Value: "sk-live-very-secret",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteString("noise line\n")
	}
	b.WriteString("OPENAI_API_KEY=sk-live-very-secret\nthe end")
	h.decideCommand("env; false")
	h.outputs["env; false"] = b.String()
	h.codes["env; false"] = 1

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "run `env; false` on jet01")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Contains(reply, "sk-live-very-secret") {
		t.Error("reply echoes a credential value")
	}
	if !strings.Contains(reply, "[redacted]") || !strings.Contains(reply, "the end") || !strings.Contains(reply, "exit 1") {
		t.Errorf("reply = ...%s", reply[max(0, len(reply)-300):])
	}
	if len(reply) > 20000 {
		t.Errorf("reply is %d bytes; output must be capped", len(reply))
	}
	tasks := h.commandTasks(t)
	if len(tasks) != 1 || tasks[0].ExitCode == nil || *tasks[0].ExitCode != 1 {
		t.Errorf("command task = %+v, want exit code 1 recorded", tasks)
	}
}

// Commands run in a per-target shell workspace that is reused, and never
// offered to the routing model as a place to dispatch agent work.
func TestDispatch_RunCommand_ShellWorkspaceReusedAndHidden(t *testing.T) {
	h := newCommandHarness(t)
	h.decideCommand("uptime")
	h.outputs["uptime"] = "up"
	for i := 0; i < 2; i++ {
		if _, err := h.r.Dispatch(context.Background(), "conv-1", "run `uptime` on jet01"); err != nil {
			t.Fatalf("Dispatch %d: %v", i, err)
		}
	}
	if ws := h.workspaces(t); len(ws) != 1 {
		t.Fatalf("workspaces = %+v, want one shell workspace reused", ws)
	}
	var offered []router.WorkspaceSnapshot
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		offered = workspaces
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(offered) != 0 {
		t.Errorf("shell workspace offered to the routing model: %+v", offered)
	}
}

func TestDispatch_RunCommand_UnknownTargetRefused(t *testing.T) {
	h := newCommandHarness(t)
	h.decide(router.Decision{Action: router.ActionRunCommand, TargetID: "nope", Command: "uptime"})
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "run `uptime` on nope"); err == nil {
		t.Fatal("Dispatch: got nil error for an unknown target")
	}
	if len(h.workspaces(t)) != 0 || len(h.exec.launchedCommands()) != 0 {
		t.Error("an unknown target got a workspace or a session")
	}
}

// A command still running at the time limit is left running for a human
// and reported as such, not killed and not an error.
func TestDispatch_RunCommand_StillRunningAtLimit(t *testing.T) {
	h := newCommandHarness(t, router.WithCommandTimeout(200*time.Millisecond))
	h.decideCommand("sleep 600")

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "run `sleep 600` on jet01")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	tasks := h.commandTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if !strings.Contains(reply, "still running") || !strings.Contains(reply, tasks[0].TmuxSession) {
		t.Errorf("reply = %q, want it to say the command is still running and where", reply)
	}
	if s := h.exec.sessionFor(tasks[0].TmuxSession); !s.alive {
		t.Error("the still-running command's session was killed")
	}
}

// Review 3: the target is the routing model's choice. The verbatim path
// runs only when the user named that same target; otherwise the user
// confirms, shown which target it would run on.
func TestDispatch_RunCommand_TargetMustBeNamedByUser(t *testing.T) {
	h := newCommandHarness(t)
	other := &registry.Target{ID: uuid.NewString(), Name: "bigbox", Kind: registry.TargetKindLocal}
	if err := h.store.CreateTarget(context.Background(), other); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	h.decideCommand("uptime") // the model picks jet01
	h.outputs["uptime"] = "up"

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "run `uptime` on bigbox")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 0 {
		t.Fatalf("ran %v on a target the user didn't name", cmds)
	}
	if !strings.Contains(reply, "jet01") || !strings.Contains(reply, `"yes"`) {
		t.Errorf("reply = %q, want a confirmation naming the target it would actually run on", reply)
	}
}

// Re-review 3: target names are case-sensitive (targets.name is UNIQUE
// as written), so "JET01" doesn't name the target "jet01": confirm first.
func TestDispatch_RunCommand_TargetNameIsCaseSensitive(t *testing.T) {
	h := newCommandHarness(t)
	h.decideCommand("uptime") // the harness target is "jet01"
	h.outputs["uptime"] = "up"

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "run `uptime` on JET01")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 0 {
		t.Fatalf("ran %v on jet01 for a message naming JET01", cmds)
	}
	if !strings.Contains(reply, `"yes"`) || !strings.Contains(reply, "jet01") {
		t.Errorf("reply = %q, want a confirmation naming jet01", reply)
	}

	if _, err := h.r.Dispatch(context.Background(), "conv-2", "run `uptime` on jet01"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 1 {
		t.Errorf("exact-case target name didn't run at once: %v", cmds)
	}
}

// LOOM-133: a command's output is shown in chat and goes to the routing
// model as history: secrets the vault doesn't hold are recognised there
// too.
func TestDispatch_RunCommand_OutputPatternRedacted(t *testing.T) {
	h := newCommandHarness(t)
	h.decideCommand("cat ~/.config/gh/hosts.yml")
	h.outputs["cat ~/.config/gh/hosts.yml"] = "github.com:\n  oauth_token: ghp_1234567890abcdefghijABCDEFGHIJ123456\n"
	reply, err := h.r.Dispatch(context.Background(), "conv-1", "run `cat ~/.config/gh/hosts.yml` on jet01")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Contains(reply, "ghp_1234567890") {
		t.Errorf("reply carries the token:\n%s", reply)
	}
}
