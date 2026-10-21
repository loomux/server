// Package routereval scores one router tier against a fixed set of
// routing and relay decisions (LOOM-187): accuracy, latency, tokens and
// cost, live against any configured endpoint or offline against a fake.
// It drives llmrouter.Model itself, so a decision is made exactly as in
// production — same prompts, schemas, validation and corrective retry —
// and scored as the router acts on it (after router.ApplyAffinity).
package routereval

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/Loomux/server/router"
)

//go:embed fixtures/escalation.json
var defaultFixtures []byte

// Fixtures is a set of cases over one world of agent types, workspaces
// and targets. It's synthetic: no user data.
type Fixtures struct {
	Version     int          `json:"version"`
	Description string       `json:"description"`
	World       World        `json:"world"`
	Decide      []DecideCase `json:"decide"`
	Relay       []RelayCase  `json:"relay"`
}

// World is what every Decide case sees.
type World struct {
	AgentTypes []AgentType `json:"agent_types"`
	Workspaces []Workspace `json:"workspaces"`
	Targets    []Target    `json:"targets"`
}

type AgentType struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type Workspace struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	Target      string   `json:"target"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Summary     string   `json:"summary"`
	// LastUsedHoursAgo is relative to the run's start.
	LastUsedHoursAgo float64 `json:"last_used_hours_ago"`
}

type Target struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	Agents        map[string]bool   `json:"agents"`
	AgentVersions map[string]string `json:"agent_versions"`
	Problem       string            `json:"problem"`
}

// DecideCase is one chat message and its conversation, with the
// decision expected of it.
type DecideCase struct {
	Name              string                    `json:"name"`
	Category          string                    `json:"category"`
	Message           string                    `json:"message"`
	History           []router.ConversationTurn `json:"history"`
	OpenTask          *OpenTask                 `json:"open_task"`
	LastWorkspaceID   string                    `json:"last_workspace_id"`
	LastWorkspaceName string                    `json:"last_workspace_name"`
	Expect            DecideExpect              `json:"expect"`
}

// OpenTask is the conversation's task awaiting input.
type OpenTask struct {
	WorkspaceID string `json:"workspace_id"`
	AgentType   string `json:"agent_type"`
	Status      string `json:"status"`
	LastReply   string `json:"last_reply"`
}

// DecideExpect is what a decision must have; empty fields aren't
// checked. All but LeaveOpenTask are checked on the decision the router
// acts on, after router.ApplyAffinity; LeaveOpenTask on the model's own.
type DecideExpect struct {
	Action      router.DecisionAction `json:"action"`
	WorkspaceID string                `json:"workspace_id"`
	AgentType   string                `json:"agent_type"`
	TargetID    string                `json:"target_id"`
	// CommandAny: the command contains at least one of these.
	CommandAny []string `json:"command_any"`
	// ProvisionTargetAny: the new workspace is on one of these.
	ProvisionTargetAny []string             `json:"provision_target_any"`
	ProvisionKind      router.ProvisionKind `json:"provision_kind"`
	GitRemote          string               `json:"git_remote"`
	LeaveOpenTask      *bool                `json:"leave_open_task"`
}

// RelayCase is one captured agent turn to condense.
type RelayCase struct {
	Name            string `json:"name"`
	AgentType       string `json:"agent_type"`
	Captured        string `json:"captured"`
	UserMessage     string `json:"user_message"`
	PreviousSummary string `json:"previous_summary"`
	// ExpectDone is the done signal the router would act on (the
	// model's, unless the reply asks the user something); nil: either.
	ExpectDone *bool `json:"expect_done"`
	// ReplyHasAny: the reply contains at least one, case-insensitively.
	ReplyHasAny []string `json:"reply_has_any"`
}

// DefaultFixtures is the built-in set (fixtures/escalation.json).
func DefaultFixtures() (*Fixtures, error) { return parseFixtures(defaultFixtures) }

// LoadFixtures reads and checks a fixture file.
func LoadFixtures(path string) (*Fixtures, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseFixtures(data)
}

func parseFixtures(data []byte) (*Fixtures, error) {
	var f Fixtures
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("routereval: fixtures: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate checks that names are unique and that every case refers only
// to the world's agent types, workspaces and targets.
func (f *Fixtures) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	agents, workspaces, targets := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, a := range f.World.AgentTypes {
		agents[a.Name] = true
	}
	for _, w := range f.World.Workspaces {
		workspaces[w.ID] = true
	}
	for _, t := range f.World.Targets {
		targets[t.ID] = true
	}
	names := map[string]bool{}
	unique := func(name string) {
		if name == "" || names[name] {
			bad("case name %q is empty or repeated", name)
		}
		names[name] = true
	}
	for _, c := range f.Decide {
		unique(c.Name)
		e := c.Expect
		if !slices.Contains([]router.DecisionAction{router.ActionAnswerDirectly, router.ActionUseWorkspace,
			router.ActionProvisionWorkspace, router.ActionRunCommand}, e.Action) {
			bad("%s: unknown action %q", c.Name, e.Action)
		}
		if c.Message == "" {
			bad("%s: no message", c.Name)
		}
		if e.WorkspaceID != "" && !workspaces[e.WorkspaceID] {
			bad("%s: unknown workspace %q", c.Name, e.WorkspaceID)
		}
		if e.AgentType != "" && !agents[e.AgentType] {
			bad("%s: unknown agent type %q", c.Name, e.AgentType)
		}
		for _, t := range append([]string{e.TargetID}, e.ProvisionTargetAny...) {
			if t != "" && !targets[t] {
				bad("%s: unknown target %q", c.Name, t)
			}
		}
		if c.OpenTask != nil && (!workspaces[c.OpenTask.WorkspaceID] || !agents[c.OpenTask.AgentType]) {
			bad("%s: open task names an unknown workspace or agent type", c.Name)
		}
		if c.LastWorkspaceID != "" && !workspaces[c.LastWorkspaceID] {
			bad("%s: unknown last workspace %q", c.Name, c.LastWorkspaceID)
		}
	}
	for _, c := range f.Relay {
		unique(c.Name)
		if c.Captured == "" {
			bad("%s: nothing captured", c.Name)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("routereval: fixtures: %w", errors.Join(errs...))
	}
	return nil
}

// agentTypes lists the world's agent type names and descriptions.
func (w World) agentTypes() ([]string, map[string]string) {
	names := make([]string, len(w.AgentTypes))
	desc := map[string]string{}
	for i, a := range w.AgentTypes {
		names[i] = a.Name
		desc[a.Name] = a.Description
	}
	return names, desc
}

// snapshots is the world as router snapshots, last-used times relative
// to now.
func (w World) snapshots(now time.Time) ([]router.WorkspaceSnapshot, []router.TargetSnapshot) {
	ws := make([]router.WorkspaceSnapshot, len(w.Workspaces))
	for i, x := range w.Workspaces {
		used := now.Add(-time.Duration(x.LastUsedHoursAgo * float64(time.Hour)))
		ws[i] = router.WorkspaceSnapshot{ID: x.ID, Name: x.Name, Status: x.Status, TargetName: x.Target,
			Description: x.Description, Tags: x.Tags, Summary: x.Summary, LastUsed: &used}
	}
	ts := make([]router.TargetSnapshot, len(w.Targets))
	for i, x := range w.Targets {
		ts[i] = router.TargetSnapshot{ID: x.ID, Name: x.Name, Kind: x.Kind, Agents: x.Agents,
			AgentVersions: x.AgentVersions, Problem: x.Problem}
	}
	return ws, ts
}

// options is c's conversation as the router passes it to Decide.
func (c DecideCase) options(w World) router.DispatchOptions {
	o := router.DispatchOptions{History: c.History, LastWorkspaceID: c.LastWorkspaceID, LastWorkspaceName: c.LastWorkspaceName}
	if c.OpenTask != nil {
		name := ""
		for _, ws := range w.Workspaces {
			if ws.ID == c.OpenTask.WorkspaceID {
				name = ws.Name
			}
		}
		o.OpenTask = &router.OpenTaskSnapshot{TaskID: "task-" + c.Name, WorkspaceID: c.OpenTask.WorkspaceID,
			WorkspaceName: name, AgentType: c.OpenTask.AgentType, Status: c.OpenTask.Status, LastReply: c.OpenTask.LastReply}
	}
	return o
}
