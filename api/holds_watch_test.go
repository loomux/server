package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// LOOM-271 (the terminal client's S10): watch holds
// (docs/design/tui-client-design.md, "Watching"). mode: "watch" on
// takeover gives a hold that only says who is looking: any number per
// task beside the one drive hold, the task's status untouched. Watch
// holds are shown beside the drive hold: watch_holds on tasks,
// attach-info and the task_hold event, and a watchers count on list rows.

// watchAttach is a takeover's attach body, as far as these tests look.
type watchAttach struct {
	TaskID        string `json:"task_id"`
	TmuxSession   string `json:"tmux_session"`
	TmuxSocket    string `json:"tmux_socket"`
	AttachCommand string `json:"attach_command"`
	// AttachCommands is raw, so a test can tell [] from null.
	AttachCommands json.RawMessage `json:"attach_commands"`
	Hold           *holdJSON       `json:"hold"`
	WatchHolds     []holdJSON      `json:"watch_holds"`
	Target         struct {
		Name string `json:"name"`
	} `json:"target"`
}

type watchAnswer struct {
	TaskID string       `json:"task_id"`
	Hold   *holdJSON    `json:"hold"`
	Attach *watchAttach `json:"attach"`
	Error  string       `json:"error"`
	Code   string       `json:"code"`
}

// takeover POSTs a takeover of taskID and returns the answer, decoded
// and raw.
func (h *holdServer) takeover(taskID, token string, body any) (int, watchAnswer, string) {
	h.t.Helper()
	status, raw := doJSON(h.t, http.MethodPost, h.base+"/api/v1/tasks/"+taskID+"/takeover", token, body)
	var out watchAnswer
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		h.t.Fatalf("takeover of %s: status %d, body %q: %v", taskID, status, raw, err)
	}
	return status, out, raw
}

// watch takes a watch hold on taskID and fails the test unless the
// answer is one.
func (h *holdServer) watch(taskID, token, label string) *holdJSON {
	h.t.Helper()
	status, out, raw := h.takeover(taskID, token, map[string]any{"mode": "watch", "client_label": label})
	if status != http.StatusOK || out.Hold == nil || out.Hold.Mode != "watch" {
		h.t.Fatalf("watch of %s = %d %s; want 200 with a watch hold", taskID, status, raw)
	}
	return out.Hold
}

func holdIDs(holds []holdJSON) []string {
	ids := make([]string, 0, len(holds))
	for _, h := range holds {
		ids = append(ids, h.ID)
	}
	return ids
}

func sameIDs(got []holdJSON, want ...string) bool {
	ids := holdIDs(got)
	if len(ids) != len(want) {
		return false
	}
	for i := range ids {
		if ids[i] != want[i] {
			return false
		}
	}
	return true
}

// A watch takeover answers with a watch hold and leaves the task as it
// was: its status and prompt, nobody driving it, nobody detached.
func TestTakeover_Watch(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusNeedsAttention, "")
	before := time.Now().Add(-time.Second)

	status, out, raw := h.takeover("t1", h.tokenA, map[string]any{"mode": "watch", "client_label": "laptop"})
	if status != http.StatusOK {
		t.Fatalf("watch = %d %s", status, raw)
	}
	hold := out.Hold
	if out.TaskID != "t1" || hold == nil || hold.ID == "" || hold.Mode != "watch" || hold.TaskID != "t1" ||
		hold.SessionID != h.sessionID(h.tokenA) || hold.ClientLabel != "laptop" || hold.PrevStatus != "needs_attention" ||
		hold.TTLSeconds != 45 || hold.EndedAt != nil || hold.AcquiredAt.Before(before) ||
		!hold.ExpiresAt.Equal(hold.AcquiredAt.Add(45*time.Second)) {
		t.Errorf("watch hold = %+v", hold)
	}
	if out.Attach == nil || out.Attach.TaskID != "t1" || out.Attach.TmuxSession != "loomux-t1" || out.Attach.TmuxSocket != "loomux-test" ||
		out.Attach.Target.Name != "jet01" {
		t.Errorf("watch attach = %+v", out.Attach)
	}
	// attach shows the task's holds as they now are: nobody drives it,
	// and this session watches it.
	if out.Attach.Hold != nil || !sameIDs(out.Attach.WatchHolds, hold.ID) {
		t.Errorf("watch attach: hold %+v, watch_holds %v; want no drive hold and the watch hold just taken", out.Attach.Hold, holdIDs(out.Attach.WatchHolds))
	}
	task, _ := h.store.GetTask(context.Background(), "t1")
	if task.Status != registry.TaskStatusNeedsAttention || task.Attention == nil {
		t.Fatalf("task after a watch = %s, attention %+v; want it untouched", task.Status, task.Attention)
	}
	if h.held("t1") {
		t.Error("a watch left the task with a drive hold")
	}
	if got := h.exec.sessions(); len(got) != 0 {
		t.Errorf("a watch detached %v", got)
	}

	// The lease is the watcher's to renew, and nobody else's.
	time.Sleep(10 * time.Millisecond)
	status, beat := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": hold.ID})
	if status != http.StatusOK || beat.Hold == nil || beat.Hold.ID != hold.ID || beat.Hold.Mode != "watch" || !beat.Hold.RenewedAt.After(hold.RenewedAt) {
		t.Errorf("heartbeat of a watch hold = %d %+v", status, beat.Hold)
	}
	if status, refused := h.post("t1/takeover/heartbeat", h.tokenB, map[string]string{"hold_id": hold.ID}); status != http.StatusForbidden {
		t.Errorf("another session's heartbeat of a watch hold = %d %+v, want 403", status, refused)
	}
	// Asking again is the same hold, renewed.
	status, again, raw := h.takeover("t1", h.tokenA, map[string]any{"mode": "watch", "ttl_seconds": 300})
	if status != http.StatusOK || again.Hold == nil || again.Hold.ID != hold.ID || again.Hold.TTLSeconds != 45 || !sameIDs(again.Attach.WatchHolds, hold.ID) {
		t.Errorf("same session watching again = %d %s; want its hold renewed", status, raw)
	}

	// Letting go ends the hold and nothing else.
	status, rel := h.post("t1/release", h.tokenA, map[string]string{"hold_id": hold.ID})
	if status != http.StatusOK || rel.Status != "needs_attention" || rel.Hold == nil || rel.Hold.ID != hold.ID || rel.Hold.EndedReason != "released" {
		t.Fatalf("release of a watch hold = %d %+v (hold %+v)", status, rel, rel.Hold)
	}
	task, _ = h.store.GetTask(context.Background(), "t1")
	if task.Status != registry.TaskStatusNeedsAttention || task.Attention == nil {
		t.Errorf("task after the watcher let go = %s, attention %+v; want it untouched", task.Status, task.Attention)
	}
	status, gone := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": hold.ID})
	if status != http.StatusGone || gone.Code != "hold_ended" || gone.EndedReason != "released" {
		t.Errorf("heartbeat of a released watch hold = %d %+v", status, gone)
	}
	if got := h.exec.sessions(); len(got) != 0 {
		t.Errorf("a watch hold's release detached %v", got)
	}
}

// A watch is never refused because somebody holds the task, and takes
// nothing from them; watching a task gives no way into its pane.
func TestTakeover_WatchBesideADriver(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	_, drive, _ := h.takeover("t1", h.tokenA, map[string]any{"client_label": "desk"})
	if drive.Hold == nil || drive.Hold.Mode != "drive" {
		t.Fatalf("drive takeover = %+v", drive)
	}

	status, out, raw := h.takeover("t1", h.tokenB, map[string]any{"mode": "watch", "client_label": "phone"})
	if status != http.StatusOK || out.Hold == nil || out.Hold.Mode != "watch" || out.Hold.ID == drive.Hold.ID || out.Hold.PrevStatus != "human_takeover" {
		t.Fatalf("watch of a driven task = %d %s; want a watch hold, not 409 task_held", status, raw)
	}
	if out.Attach.Hold == nil || out.Attach.Hold.ID != drive.Hold.ID || !sameIDs(out.Attach.WatchHolds, out.Hold.ID) {
		t.Errorf("watch attach: hold %+v, watch_holds %v; want the driver's hold and the watcher's", out.Attach.Hold, holdIDs(out.Attach.WatchHolds))
	}
	if got := h.status("t1"); got != registry.TaskStatusHumanTakeover {
		t.Errorf("status = %s, want human-takeover", got)
	}
	if got := h.exec.sessions(); len(got) != 0 {
		t.Errorf("a watch detached the driver: %v", got)
	}
	if status, beat := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": drive.Hold.ID}); status != http.StatusOK {
		t.Errorf("the driver's heartbeat after a watch = %d %+v", status, beat)
	}
	// The watcher's own drive takeover is refused as held, like anyone's.
	status, refused, _ := h.takeover("t1", h.tokenB, nil)
	if status != http.StatusConflict || refused.Code != "task_held" || refused.Hold == nil || refused.Hold.ID != drive.Hold.ID {
		t.Errorf("the watcher's drive takeover = %d %+v; want 409 task_held with the driver's hold", status, refused)
	}
	// The driver's takeover gets its drive hold back, and attach still
	// shows who watches.
	status, mine, _ := h.takeover("t1", h.tokenA, nil)
	if status != http.StatusOK || mine.Hold.ID != drive.Hold.ID || mine.Attach.Hold == nil || mine.Attach.Hold.ID != drive.Hold.ID ||
		!sameIDs(mine.Attach.WatchHolds, out.Hold.ID) {
		t.Errorf("the driver's takeover = %d hold %+v, attach hold %+v, watch_holds %v", status, mine.Hold, mine.Attach.Hold, holdIDs(mine.Attach.WatchHolds))
	}

	// The watcher letting go leaves the task with its driver.
	if status, rel := h.post("t1/release", h.tokenB, map[string]string{"hold_id": out.Hold.ID}); status != http.StatusOK || rel.Status != "human_takeover" {
		t.Errorf("release of the watch hold = %d %+v; want the task still human_takeover", status, rel)
	}
	if !h.held("t1") || h.status("t1") != registry.TaskStatusHumanTakeover {
		t.Error("the watcher's release took the task from its driver")
	}
	// A Take back ends the drive hold and leaves a watcher's.
	w := h.watch("t1", h.tokenB, "phone")
	if status, back := h.post("t1/release", h.tokenB, nil); status != http.StatusOK || back.Hold == nil || back.Hold.ID != drive.Hold.ID || back.Hold.EndedReason != "taken_back" {
		t.Fatalf("take back = %d %+v", status, back)
	}
	if status, beat := h.post("t1/takeover/heartbeat", h.tokenB, map[string]string{"hold_id": w.ID}); status != http.StatusOK {
		t.Errorf("the watcher's heartbeat after a take back = %d %+v; its hold must stand", status, beat)
	}
}

// watchCommand is one of a watch answer's attach_commands.
type watchCommand struct {
	Via     string   `json:"via"`
	Command string   `json:"command"`
	Argv    []string `json:"argv"`
}

// watchServer is a hold server with three tasks to watch: t-machine on
// remote-1, which is a plugin's machine (stub); t-plain on a plain
// remote target; t-local on a local one.
func watchServer(t *testing.T) (*holdServer, *stubMachines) {
	t.Helper()
	stub := &stubMachines{envs: map[string]*registry.Environment{}}
	h := newHoldServer(t, api.WithMachines(stub))
	ctx := context.Background()
	stub.envs["remote-1"] = &registry.Environment{ID: "abc", TargetID: "remote-1"}
	h.task("t-machine", registry.TaskStatusAwaitingInput, "")
	for id, target := range map[string]*registry.Target{
		"plain": {ID: "plain-1", Name: "jet02", Kind: registry.TargetKindRemote, Host: "jet02.example.com", User: "orski", SSHPort: 2222},
		"local": {ID: "local-1", Name: "here", Kind: registry.TargetKindLocal},
	} {
		if err := h.store.CreateTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
		if err := h.store.CreateWorkspace(ctx, &registry.Workspace{ID: "ws-" + id, Name: "ws-" + id, Path: "/w", TargetID: target.ID, Status: registry.WorkspaceStatusActive}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.CreateTask(ctx, &registry.Task{ID: "t-" + id, WorkspaceID: "ws-" + id, Kind: registry.TaskKindAgent, AgentType: "claude-code",
			TmuxSession: "loomux-t-" + id, Status: registry.TaskStatusAwaitingInput, ConversationID: "conv-t-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	return h, stub
}

// A watch is handed only commands that attach read-only (LOOM-272): the
// local form, the ssh form and a plugin's own, each with tmux's -r. The
// server builds or checks them; nothing is left for the client to add,
// and the writable commands attach-info carries are never in the answer.
func TestTakeover_WatchCommandsAreReadOnly(t *testing.T) {
	h, _ := watchServer(t)
	for taskID, want := range map[string][]watchCommand{
		"t-machine": {
			{Via: "kubectl", Command: "kubectl exec -it lx-abc -- tmux -L loomux attach -r -t loomux-t-machine",
				Argv: []string{"kubectl", "exec", "-it", "lx-abc", "--", "tmux", "-L", "loomux", "attach", "-r", "-t", "loomux-t-machine"}},
			{Via: "ssh", Command: "ssh orski@jet01.example.com -t 'tmux -L loomux-test attach -r -t loomux-t-machine'",
				Argv: []string{"ssh", "orski@jet01.example.com", "-t", "tmux -L loomux-test attach -r -t loomux-t-machine"}},
		},
		"t-plain": {
			{Via: "ssh", Command: "ssh -p 2222 orski@jet02.example.com -t 'tmux -L loomux-test attach -r -t loomux-t-plain'",
				Argv: []string{"ssh", "-p", "2222", "orski@jet02.example.com", "-t", "tmux -L loomux-test attach -r -t loomux-t-plain"}},
		},
		"t-local": {},
	} {
		// What attach-info hands a driver, so the watch has something
		// not to be given.
		info := h.get("tasks/"+taskID+"/attach-info", h.tokenA)
		var driver watchAttach
		if err := json.Unmarshal([]byte(info), &driver); err != nil {
			t.Fatal(err)
		}
		if driver.AttachCommand != "tmux -L loomux-test attach -t loomux-"+taskID || strings.Contains(info, "attach -r") {
			t.Fatalf("%s: attach-info = %s; it stays the writable commands", taskID, info)
		}

		status, out, raw := h.takeover(taskID, h.tokenA, map[string]any{"mode": "watch"})
		if status != http.StatusOK || out.Hold == nil || out.Hold.Mode != "watch" {
			t.Fatalf("%s: watch = %d %s", taskID, status, raw)
		}
		if got, want := out.Attach.AttachCommand, "tmux -L loomux-test attach -r -t loomux-"+taskID; got != want {
			t.Errorf("%s: watch attach_command = %q, want %q", taskID, got, want)
		}
		var got []watchCommand
		if err := json.Unmarshal(out.Attach.AttachCommands, &got); err != nil || got == nil {
			t.Fatalf("%s: watch attach_commands = %s (%v); want a list", taskID, out.Attach.AttachCommands, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: watch attach_commands = %+v, want %+v", taskID, got, want)
		}
		for i := range want {
			if got[i].Via != want[i].Via || got[i].Command != want[i].Command || !slices.Equal(got[i].Argv, want[i].Argv) {
				t.Errorf("%s: watch attach_commands[%d] = %+v, want %+v", taskID, i, got[i], want[i])
			}
		}
		// Whatever the forms, nothing in the answer attaches writable.
		if strings.Contains(raw, "attach -t") || strings.Contains(raw, `"attach","-t"`) {
			t.Errorf("%s: watch answer carries a writable attach: %s", taskID, raw)
		}
		// A drive takeover of the same task still gets the writable ones.
		status, drive, raw := h.takeover(taskID, h.tokenB, nil)
		if status != http.StatusOK || drive.Attach.AttachCommand != driver.AttachCommand || strings.Contains(raw, "attach -r") {
			t.Errorf("%s: drive takeover = %d %s; want the writable commands", taskID, status, raw)
		}
	}
}

// A machine whose plugin has no read-only attach command can't be
// watched: 409 watch_unsupported, naming the plugin, and no hold is
// taken. It is never answered with the plugin's writable command, nor
// with the ssh form alone, which doesn't reach a plugin's machine.
func TestTakeover_WatchUnsupported(t *testing.T) {
	h, stub := watchServer(t)
	stub.watchErr = &providers.WatchUnsupportedError{Plugin: "kubernetes", Version: "0.3.9"}

	status, raw := doJSON(t, http.MethodPost, h.base+"/api/v1/tasks/t-machine/takeover", h.tokenA, map[string]any{"mode": "watch"})
	var out watchAnswer
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusConflict || out.Code != "watch_unsupported" || !strings.Contains(out.Error, "kubernetes") || out.Hold != nil || out.Attach != nil {
		t.Fatalf("watch of a machine whose plugin can't = %d %s; want 409 watch_unsupported naming the plugin, no hold, no attach", status, raw)
	}
	if strings.Contains(raw, "tmux") || strings.Contains(raw, "kubectl") || strings.Contains(raw, "ssh") {
		t.Errorf("the refusal carries a command: %s", raw)
	}
	if holds, _ := h.store.ListActiveTaskHolds(context.Background()); len(holds) != 0 {
		t.Errorf("a refused watch left a hold: %+v", holds[0])
	}
	if got := h.status("t-machine"); got != registry.TaskStatusAwaitingInput {
		t.Errorf("status after a refused watch = %s", got)
	}
	// Other targets are watched as before, and the machine can still be
	// driven.
	if status, _, raw := h.takeover("t-plain", h.tokenA, map[string]any{"mode": "watch"}); status != http.StatusOK {
		t.Errorf("watch of a plain target = %d %s", status, raw)
	}
	if status, drive, raw := h.takeover("t-machine", h.tokenA, nil); status != http.StatusOK || drive.Hold.Mode != "drive" || !strings.Contains(raw, "kubectl") {
		t.Errorf("drive takeover of the machine = %d %s", status, raw)
	}
}

// A plugin that can't be asked just now (stopped, not answering) leaves
// its command out, as it does from attach-info: the watch gets the ssh
// form, read-only, and never anything writable.
func TestTakeover_WatchWithThePluginDown(t *testing.T) {
	h, stub := watchServer(t)
	stub.watchErr = plugins.ErrUnavailable

	status, out, raw := h.takeover("t-machine", h.tokenA, map[string]any{"mode": "watch"})
	if status != http.StatusOK || out.Hold == nil || out.Hold.Mode != "watch" {
		t.Fatalf("watch with the plugin down = %d %s", status, raw)
	}
	var got []watchCommand
	if err := json.Unmarshal(out.Attach.AttachCommands, &got); err != nil || len(got) != 1 || got[0].Via != "ssh" ||
		!strings.HasSuffix(got[0].Command, "'tmux -L loomux-test attach -r -t loomux-t-machine'") {
		t.Errorf("watch attach_commands with the plugin down = %s; want the read-only ssh form alone", out.Attach.AttachCommands)
	}
	if strings.Contains(raw, "attach -t") {
		t.Errorf("watch answer carries a writable attach: %s", raw)
	}
}

// force takes a pane from its holder, which a watch never asks for: it
// is refused, not ignored. An ended task has no pane to watch.
func TestTakeover_WatchRefusals(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	_, drive, _ := h.takeover("t1", h.tokenA, nil)

	status, out, _ := h.takeover("t1", h.tokenB, map[string]any{"mode": "watch", "force": true})
	if status != http.StatusBadRequest || out.Code != "invalid_request" || out.Error == "" || out.Hold != nil || out.Attach != nil {
		t.Errorf("watch with force = %d %+v; want 400 invalid_request", status, out)
	}
	if got := h.exec.sessions(); len(got) != 0 {
		t.Errorf("a refused watch with force detached %v", got)
	}
	if status, beat := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": drive.Hold.ID}); status != http.StatusOK {
		t.Errorf("the driver's heartbeat after a watch with force = %d %+v", status, beat)
	}
	h.task("done", registry.TaskStatusCompleted, "")
	h.task("timeout", registry.TaskStatusFailed, registry.ErrorClassTimeout)
	for _, id := range []string{"done", "timeout"} {
		status, out, _ := h.takeover(id, h.tokenB, map[string]any{"mode": "watch"})
		if status != http.StatusConflict || out.Code != "task_inactive" || out.Hold != nil {
			t.Errorf("%s: watch = %d %+v; want 409 task_inactive", id, status, out)
		}
	}
	if status, out, _ := h.takeover("nope", h.tokenB, map[string]any{"mode": "watch"}); status != http.StatusNotFound {
		t.Errorf("watch of an unknown task = %d %+v, want 404", status, out)
	}
	// A failed task whose pane was kept can be watched, and stays failed.
	h.task("login", registry.TaskStatusFailed, registry.ErrorClassLoginRequired)
	status, out, raw := h.takeover("login", h.tokenB, map[string]any{"mode": "watch"})
	if status != http.StatusOK || out.Hold == nil || out.Hold.Mode != "watch" || out.Hold.PrevStatus != "failed" || h.status("login") != registry.TaskStatusFailed {
		t.Errorf("watch of a failed task with a kept pane = %d %s", status, raw)
	}
	holds, _ := h.store.ListActiveTaskHolds(context.Background())
	if len(holds) != 2 {
		t.Errorf("active holds = %d, want the drive hold and the one watch", len(holds))
	}
}

// A revoked session's watch holds end with it, and nobody is detached.
func TestHolds_WatchEndsWithItsSession(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	_, drive, _ := h.takeover("t1", h.tokenA, nil)
	w := h.watch("t1", h.tokenB, "phone")

	if status, raw := doJSON(t, http.MethodPost, h.base+"/api/v1/logout", h.tokenB, nil); status != http.StatusNoContent {
		t.Fatalf("logout = %d %s", status, raw)
	}
	ended, err := h.store.GetTaskHold(context.Background(), w.ID)
	if err != nil || ended.Active() || ended.EndedReason != registry.HoldEndSessionRevoked {
		t.Errorf("watch hold after its session logged out = %+v, %v; want ended as session_revoked", ended, err)
	}
	if got := h.exec.sessions(); len(got) != 0 {
		t.Errorf("a watcher's logout detached %v", got)
	}
	if status, beat := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": drive.Hold.ID}); status != http.StatusOK {
		t.Errorf("the driver's heartbeat after a watcher's logout = %d %+v", status, beat)
	}
}

// taskFields is GET /conversations/{conv}'s task taskID, by field.
func (h *holdServer) taskFields(conv, taskID string) map[string]json.RawMessage {
	h.t.Helper()
	var out struct {
		Tasks []map[string]json.RawMessage `json:"tasks"`
	}
	raw := h.get("conversations/"+conv, h.tokenA)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		h.t.Fatalf("decode conversation: %v", err)
	}
	for _, task := range out.Tasks {
		var id string
		_ = json.Unmarshal(task["id"], &id)
		if id == taskID {
			return task
		}
	}
	h.t.Fatalf("conversation %s has no task %s: %s", conv, taskID, raw)
	return nil
}

func decodeHolds(t *testing.T, raw json.RawMessage) []holdJSON {
	t.Helper()
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		t.Fatalf("watch_holds = %s, want an array (never null or absent)", raw)
	}
	var holds []holdJSON
	if err := json.Unmarshal(raw, &holds); err != nil {
		t.Fatalf("decode watch_holds %s: %v", raw, err)
	}
	return holds
}

// A conversation's tasks and attach-info carry who watches, oldest
// first, beside who drives. hold stays the drive hold: a watcher is
// never shown as the holder.
func TestConversation_TasksCarryTheirWatchHolds(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	attachFields := func() map[string]json.RawMessage {
		t.Helper()
		return rawFields(t, []byte(h.get("tasks/t1/attach-info", h.tokenA)))
	}
	check := func(when string, wantHold string, want ...string) {
		t.Helper()
		for where, fields := range map[string]map[string]json.RawMessage{"task": h.taskFields("conv-t1", "t1"), "attach-info": attachFields()} {
			if got := holdIDOf(t, fields["hold"]); got != wantHold {
				t.Errorf("%s, %s: hold = %q, want %q (the drive hold only)", when, where, got, wantHold)
			}
			holds := decodeHolds(t, fields["watch_holds"])
			if !sameIDs(holds, want...) {
				t.Errorf("%s, %s: watch_holds = %v, want %v", when, where, holdIDs(holds), want)
			}
			for _, w := range holds {
				if w.Mode != "watch" || w.TaskID != "t1" || w.EndedAt != nil {
					t.Errorf("%s, %s: watch_holds entry = %+v", when, where, w)
				}
			}
		}
	}

	check("before", "")
	wa := h.watch("t1", h.tokenA, "laptop")
	check("one watcher", "", wa.ID)
	wb := h.watch("t1", h.tokenB, "phone")
	check("two watchers", "", wa.ID, wb.ID)
	if got := decodeHolds(t, h.taskFields("conv-t1", "t1")["watch_holds"]); got[0].ClientLabel != "laptop" || got[1].ClientLabel != "phone" ||
		got[1].SessionID != h.sessionID(h.tokenB) {
		t.Errorf("watch_holds = %+v; want laptop's then phone's", got)
	}

	_, drive, _ := h.takeover("t1", h.tokenB, map[string]any{"client_label": "phone"})
	check("driven and watched", drive.Hold.ID, wa.ID, wb.ID)
	if status, _ := h.post("t1/release", h.tokenA, map[string]string{"hold_id": wa.ID}); status != http.StatusOK {
		t.Fatalf("release: %d", status)
	}
	check("a watcher gone", drive.Hold.ID, wb.ID)
	if status, _ := h.post("t1/release", h.tokenA, nil); status != http.StatusOK {
		t.Fatalf("take back: %d", status)
	}
	check("taken back", "", wb.ID)
}

// A server without holds shows nobody watching, as an empty list.
func TestConversation_WatchHoldsWithoutHolds(t *testing.T) {
	srv, _, store := newTestServer(t)
	ctx := context.Background()
	target := &registry.Target{ID: "r1", Name: "jet01", Kind: registry.TargetKindRemote, Host: "jet01.example.com", User: "orski"}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: "ws-1", Name: "ws-1", Path: "/w", TargetID: target.ID, Status: registry.WorkspaceStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, &registry.Task{ID: "t1", WorkspaceID: "ws-1", Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-t1", Status: registry.TaskStatusAwaitingInput, ConversationID: "conv-t1"}); err != nil {
		t.Fatal(err)
	}
	token, _ := login(t, srv.URL, testPassword)

	_, raw := doJSON(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-t1", token, nil)
	var conv struct {
		Tasks []map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(raw), &conv); err != nil || len(conv.Tasks) != 1 {
		t.Fatalf("conversation = %s: %v", raw, err)
	}
	if got := string(conv.Tasks[0]["watch_holds"]); got != "[]" {
		t.Errorf("task watch_holds on a server without holds = %s, want []", got)
	}
	_, raw = doJSON(t, http.MethodGet, srv.URL+"/api/v1/tasks/t1/attach-info", token, nil)
	if got := string(rawFields(t, []byte(raw))["watch_holds"]); got != "[]" {
		t.Errorf("attach-info watch_holds on a server without holds = %s, want []", got)
	}
}

// rowFields is GET /conversations' row for conv, by field.
func (h *holdServer) rowFields(conv string) map[string]json.RawMessage {
	h.t.Helper()
	var out struct {
		Conversations []map[string]json.RawMessage `json:"conversations"`
	}
	raw := h.get("conversations", h.tokenA)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		h.t.Fatal(err)
	}
	for _, row := range out.Conversations {
		var id string
		_ = json.Unmarshal(row["conversation_id"], &id)
		if id == conv {
			return row
		}
	}
	h.t.Fatalf("no row for %s in %s", conv, raw)
	return nil
}

// A list row counts who watches its conversation's tasks, beside
// held_by. A watcher isn't a holder: a row that is only watched has no
// held_by, and a row nobody watches has no watchers.
func TestListConversations_Watchers(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	h.task("t2", registry.TaskStatusAwaitingInput, "")

	if row := h.rowFields("conv-t1"); row["watchers"] != nil {
		t.Fatalf("unwatched row has watchers = %s; want it left out", row["watchers"])
	}
	wa := h.watch("t1", h.tokenA, "laptop")
	h.watch("t1", h.tokenB, "phone")
	row := h.rowFields("conv-t1")
	if got := string(row["watchers"]); got != "2" {
		t.Errorf("row watched twice: watchers = %s, want 2", got)
	}
	if row["held_by"] != nil {
		t.Errorf("row that is only watched has held_by = %s; a watcher isn't the holder", row["held_by"])
	}
	if other := h.rowFields("conv-t2"); other["watchers"] != nil {
		t.Errorf("another conversation's row has watchers = %s", other["watchers"])
	}

	// Driven too: both are shown, and held_by is the driver.
	_, drive, _ := h.takeover("t1", h.tokenB, map[string]any{"client_label": "desk"})
	row = h.rowFields("conv-t1")
	var by struct {
		ClientLabel string    `json:"client_label"`
		Since       time.Time `json:"since"`
	}
	if err := json.Unmarshal(row["held_by"], &by); err != nil || by.ClientLabel != "desk" || !by.Since.Equal(drive.Hold.AcquiredAt) {
		t.Errorf("held_by = %s (%v); want the driver, desk", row["held_by"], err)
	}
	if got := string(row["watchers"]); got != "2" {
		t.Errorf("driven and watched row: watchers = %s, want 2", got)
	}

	if status, _ := h.post("t1/release", h.tokenA, map[string]string{"hold_id": wa.ID}); status != http.StatusOK {
		t.Fatalf("release: %d", status)
	}
	if got := string(h.rowFields("conv-t1")["watchers"]); got != "1" {
		t.Errorf("after one watcher let go: watchers = %s, want 1", got)
	}
}

// taskHoldsEvent is the data of a stream's `event: task_hold`, with the
// watch holds S10 adds.
type taskHoldsEvent struct {
	TaskID      string          `json:"task_id"`
	Hold        *holdJSON       `json:"hold"`
	WatchHolds  json.RawMessage `json:"watch_holds"`
	EndedReason string          `json:"ended_reason"`
}

// nextTaskHolds reads r's events up to the next task_hold.
func nextTaskHolds(t *testing.T, r *bufio.Reader) (taskHoldsEvent, []holdJSON) {
	t.Helper()
	for {
		ev := readSSEEventWithTimeout(t, r, 3*time.Second)
		if ev.Event != "task_hold" {
			continue
		}
		var out taskHoldsEvent
		if err := json.Unmarshal([]byte(ev.Data), &out); err != nil {
			t.Fatalf("task_hold %s: %v", ev.Data, err)
		}
		return out, decodeHolds(t, out.WatchHolds)
	}
}

// The stream's task_hold covers both modes: it carries the task's drive
// hold and its watch holds as they now are, whenever either changes.
// hold and ended_reason keep their meaning (the drive hold, and why the
// one before it ended), so a watcher coming or going is never read as
// the task being taken or given back.
func TestStream_TaskHoldCoversWatchHolds(t *testing.T) {
	const poll = 20 * time.Millisecond
	h := newHoldServer(t, api.WithStreamPollInterval(poll))
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	r := openStream(t, h.base, h.tokenA, "conv-t1")

	wa := h.watch("t1", h.tokenA, "laptop")
	ev, watching := nextTaskHolds(t, r)
	if ev.TaskID != "t1" || ev.Hold != nil || ev.EndedReason != "" || !sameIDs(watching, wa.ID) || watching[0].Mode != "watch" {
		t.Fatalf("first watcher: event = %+v, watch_holds %v; want t1, no drive hold, the watch hold", ev, holdIDs(watching))
	}

	// A renewal isn't an event, for a watch hold either.
	if status, _ := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": wa.ID}); status != http.StatusOK {
		t.Fatalf("heartbeat: %d", status)
	}
	time.Sleep(10 * poll)

	wb := h.watch("t1", h.tokenB, "phone")
	ev, watching = nextTaskHolds(t, r)
	if ev.Hold != nil || ev.EndedReason != "" || !sameIDs(watching, wa.ID, wb.ID) {
		t.Fatalf("event after a heartbeat and a second watcher = %+v, watch_holds %v; want both watch holds", ev, holdIDs(watching))
	}

	_, drive, _ := h.takeover("t1", h.tokenB, map[string]any{"client_label": "phone"})
	ev, watching = nextTaskHolds(t, r)
	if ev.Hold == nil || ev.Hold.ID != drive.Hold.ID || ev.Hold.Mode != "drive" || ev.EndedReason != "" || !sameIDs(watching, wa.ID, wb.ID) {
		t.Fatalf("drive takeover: event = %+v, watch_holds %v; want the drive hold and both watchers", ev, holdIDs(watching))
	}

	if status, _ := h.post("t1/release", h.tokenA, map[string]string{"hold_id": wa.ID}); status != http.StatusOK {
		t.Fatalf("release of the watch hold: %d", status)
	}
	ev, watching = nextTaskHolds(t, r)
	if ev.Hold == nil || ev.Hold.ID != drive.Hold.ID || ev.EndedReason != "" || !sameIDs(watching, wb.ID) {
		t.Fatalf("a watcher gone: event = %+v, watch_holds %v; want the same drive hold, no ended_reason, one watcher", ev, holdIDs(watching))
	}

	if status, _ := h.post("t1/release", h.tokenA, nil); status != http.StatusOK {
		t.Fatalf("take back: %d", status)
	}
	ev, watching = nextTaskHolds(t, r)
	if ev.Hold != nil || ev.EndedReason != "taken_back" || !sameIDs(watching, wb.ID) {
		t.Fatalf("take back: event = %+v, watch_holds %v; want null, taken_back, the watcher still there", ev, holdIDs(watching))
	}

	if status, _ := h.post("t1/release", h.tokenB, map[string]string{"hold_id": wb.ID}); status != http.StatusOK {
		t.Fatalf("release of the last watch hold: %d", status)
	}
	ev, watching = nextTaskHolds(t, r)
	if ev.Hold != nil || ev.EndedReason != "" || len(watching) != 0 {
		t.Fatalf("last watcher gone: event = %+v, watch_holds %v; want null, no ended_reason, []", ev, holdIDs(watching))
	}
}

// Watch holds there when the stream connects are the past, like the
// drive hold.
func TestStream_TaskHoldWatchBaseline(t *testing.T) {
	const poll = 20 * time.Millisecond
	h := newHoldServer(t, api.WithStreamPollInterval(poll))
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	wa := h.watch("t1", h.tokenA, "laptop")
	wb := h.watch("t1", h.tokenB, "phone")
	r := openStream(t, h.base, h.tokenA, "conv-t1")
	time.Sleep(10 * poll)
	if status, _ := h.post("t1/release", h.tokenA, map[string]string{"hold_id": wa.ID}); status != http.StatusOK {
		t.Fatalf("release: %d", status)
	}
	ev, watching := nextTaskHolds(t, r)
	if ev.Hold != nil || ev.EndedReason != "" || !sameIDs(watching, wb.ID) {
		t.Fatalf("first task_hold = %+v, watch_holds %v; want the release (one watcher left), not the holds there at connect", ev, holdIDs(watching))
	}
}

// driveForWatch is a hold service that ignores the mode asked for and
// takes a drive hold, as a server that didn't know `mode` would.
type driveForWatch struct{ *orchestrator.Orchestrator }

func (d driveForWatch) Takeover(ctx context.Context, req orchestrator.TakeoverRequest) (*orchestrator.TakeoverResult, error) {
	req.Mode = registry.HoldModeDrive
	return d.Orchestrator.Takeover(ctx, req)
}

// A session that asked to watch is never answered with another kind of
// hold, whatever the hold service did: no hold, no attach, and the hold
// it wrongly got is given back rather than left pausing the task.
func TestTakeover_WatchNeverAnsweredWithADriveHold(t *testing.T) {
	exec := &detachRecorder{}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option {
		return []api.Option{api.WithTaskHolds(driveForWatch{orchestrator.New(s, factory, nil)}), api.WithTmuxSocket("loomux-test")}
	})
	ctx := context.Background()
	if err := store.CreateTarget(ctx, &registry.Target{ID: "remote-1", Name: "jet01", Kind: registry.TargetKindRemote, Host: "jet01.example.com", User: "orski"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: "ws-1", Name: "ws-1", Path: "/srv/app", TargetID: "remote-1", Status: registry.WorkspaceStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, &registry.Task{ID: "t1", WorkspaceID: "ws-1", Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-t1", Status: registry.TaskStatusAwaitingInput, ConversationID: "conv-t1"}); err != nil {
		t.Fatal(err)
	}
	token, _ := login(t, srv.URL, testPassword)

	status, raw := doJSON(t, http.MethodPost, srv.URL+"/api/v1/tasks/t1/takeover", token, map[string]any{"mode": "watch"})
	if status != http.StatusInternalServerError || strings.Contains(raw, "attach") || strings.Contains(raw, `"hold"`) {
		t.Fatalf("watch answered by a drive hold = %d %s; want 500 with no hold and no attach", status, raw)
	}
	if holds, _ := store.ListActiveTaskHolds(ctx); len(holds) != 0 {
		t.Errorf("the hold wrongly taken is still active: %+v", holds[0])
	}
	if task, _ := store.GetTask(ctx, "t1"); task.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("status = %s; want the task handed back", task.Status)
	}
}
