package llmrouter

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Loomux/server/router"
)

var testOffers = []router.MachineOffer{{
	PluginID: "plug-1", Plugin: "kubernetes", Label: "test cluster",
	Sizes: []router.MachineSize{{Name: "small", CPU: "1", Memory: "2Gi", Disk: "10Gi"},
		{Name: "medium", CPU: "2", Memory: "4Gi", Disk: "20Gi"}},
	PersistentDefault: true, Persistent: true, Ephemeral: true,
	Egress: []string{"internet", "none", "trusted"}, Machines: 1, MaxMachines: 5,
}, {
	PluginID: "plug-2", Plugin: "docker", Sizes: []router.MachineSize{{Name: "large", CPU: "4", Memory: "8Gi", Disk: "40Gi"}},
	PersistentDefault: true, Persistent: true,
}}

// LOOM-216: provision_target is in the schema only with a machine plugin
// to offer, its fields bound to what the offers list.
func TestDecideTool_ProvisionTarget(t *testing.T) {
	actions := func(tool toolSpec) []string {
		return tool.schema["properties"].(map[string]any)["action"].(map[string]any)["enum"].([]string)
	}
	without := decideTool([]string{"claude-code"}, nil, []string{"t-1"}, false, nil)
	if slices.Contains(actions(without), "provision_target") {
		t.Errorf("offered without a machine plugin: %v", actions(without))
	}
	if _, ok := without.schema["properties"].(map[string]any)["new_target"]; ok {
		t.Error("new_target in the schema without a machine plugin")
	}

	// No target registered, a plugin to make one: provision_target alone
	// besides the two that need none.
	tool := decideTool([]string{"claude-code"}, nil, nil, false, testOffers)
	if got := actions(tool); !reflect.DeepEqual(got, []string{"answer_directly", "use_workspace", "provision_target"}) {
		t.Errorf("actions = %v", got)
	}
	nt := tool.schema["properties"].(map[string]any)["new_target"].(map[string]any)["properties"].(map[string]any)
	enum := func(field string) []string { e, _ := nt[field].(map[string]any)["enum"].([]string); return e }
	if got := enum("plugin_id"); !reflect.DeepEqual(got, []string{"plug-1", "plug-2"}) {
		t.Errorf("plugin_id enum = %v", got)
	}
	if got := enum("size"); !reflect.DeepEqual(got, []string{"small", "medium", "large"}) {
		t.Errorf("size enum = %v", got)
	}
	if got := enum("egress"); !reflect.DeepEqual(got, []string{"internet", "none", "trusted"}) {
		t.Errorf("egress enum = %v", got)
	}
	if nt["name"].(map[string]any)["pattern"] != router.WorkspaceNamePattern {
		t.Errorf("name = %+v", nt["name"])
	}
}

// Each plugin's own lists bound its proposal; the schema's enums are the
// union.
func TestValidateTarget(t *testing.T) {
	yes := true
	d, err := validateTarget(decideArgumentsTarget{PluginID: "plug-1", Name: "builds", Size: "medium", KeepData: &yes, Egress: "none"}, testOffers)
	want := router.Decision{Action: router.ActionProvisionTarget, NewTarget: router.MachineSpec{
		PluginID: "plug-1", Name: "builds", Size: "medium", Persistent: &yes, Egress: "none"}}
	if err != nil || !reflect.DeepEqual(d, want) {
		t.Errorf("decision = %+v, %v", d, err)
	}
	if d, err := validateTarget(decideArgumentsTarget{PluginID: "plug-2", Name: "box"}, testOffers); err != nil || d.NewTarget.Size != "" || d.NewTarget.Persistent != nil {
		t.Errorf("defaults left to the plugin: %+v, %v", d, err)
	}
	for name, bad := range map[string]decideArgumentsTarget{
		"unknown plugin":         {PluginID: "plug-9", Name: "x"},
		"no name":                {PluginID: "plug-1"},
		"not a slug":             {PluginID: "plug-1", Name: "My Box"},
		"another plugin's size":  {PluginID: "plug-1", Name: "x", Size: "large"},
		"an egress not offered":  {PluginID: "plug-2", Name: "x", Egress: "none"},
		"trusted where none is":  {PluginID: "plug-2", Name: "x", Egress: "trusted"},
		"a size nobody offers":   {PluginID: "plug-1", Name: "x", Size: "huge"},
		"an egress nobody knows": {PluginID: "plug-1", Name: "x", Egress: "lan"},
	} {
		if _, err := validateTarget(bad, testOffers); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecideUserPrompt_Machines(t *testing.T) {
	prompt := decideUserPrompt("a new machine please", nil, nil, router.DispatchOptions{Machines: testOffers})
	for _, want := range []string{
		"Machine plugins (a new machine can be created with provision_target)",
		"- plugin_id: plug-1\n  plugin: kubernetes (test cluster)",
		"sizes: small (1 CPU, 2Gi memory, 10Gi disk), medium (2 CPU, 4Gi memory, 20Gi disk)",
		"data: kept by default; makes both kinds",
		"egress: internet, none, trusted",
		"machines: 1 of at most 5",
		"data: kept by default; only machines that keep their data",
		"egress: no choice (internet)",
		"propose one with provision_target",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	none := decideUserPrompt("a new machine please", nil, nil, router.DispatchOptions{})
	if !strings.Contains(none, "Machine plugins: (none)") || !strings.Contains(none, "a machine plugin (Kubernetes or Docker) must be installed first") {
		t.Errorf("prompt without a plugin:\n%s", none)
	}
	if strings.Contains(none, "provision_target") {
		t.Errorf("provision_target named without a plugin:\n%s", none)
	}
}

// withOffers gives a Decide call machine offers, as Router.Dispatch does.
func withOffers(offers []router.MachineOffer) router.DispatchOption {
	return func(o *router.DispatchOptions) { o.Machines = offers }
}

// A name that isn't a slug is fixed by the corrective retry, told the
// pattern, on the same tier (LOOM-107).
func TestDecide_ProvisionTargetBadName_RecoversByCorrectiveRetry(t *testing.T) {
	h, bodies := sequenceHandler(
		toolCallHandler(t, decideToolName, map[string]any{"action": "provision_target",
			"new_target": map[string]any{"plugin_id": "plug-1", "name": "Build Box"}}),
		toolCallHandler(t, decideToolName, map[string]any{"action": "provision_target",
			"new_target": map[string]any{"plugin_id": "plug-1", "name": "build-box"}}),
	)
	primarySrv, primaryCount := newFakeServer(t, h)
	escalationSrv, escalationCount := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "from escalation"}))
	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, []string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := m.Decide(context.Background(), "a new machine for builds", nil, nil, withOffers(testOffers))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Action != router.ActionProvisionTarget || dec.NewTarget.Name != "build-box" || dec.NewTarget.PluginID != "plug-1" {
		t.Fatalf("Decide = %+v, want provision_target build-box", dec)
	}
	if *primaryCount != 2 || *escalationCount != 0 {
		t.Errorf("primary called %d times, escalation %d; want 2 and 0", *primaryCount, *escalationCount)
	}
	if b := bodies(); len(b) != 2 || !strings.Contains(b[1], "was rejected") || !strings.Contains(b[1], "Build Box") {
		t.Errorf("the retry doesn't say what was wrong: %v", b)
	}
}

// With no machine plugin, a provision_target the model picks anyway is
// answered with how to get one, as LOOM-68 answers a provisioning with
// no target: no corrective retry, no escalation, and an open task is
// left alone, since it was a request for a machine.
func TestDecide_ProvisionTargetWithoutPlugin_Answers(t *testing.T) {
	primarySrv, primaryCount := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{"action": "provision_target",
		"new_target": map[string]any{"plugin_id": "plug-1", "name": "builds"}, "leave_open_task": false}))
	escalationSrv, escalationCount := newFakeServer(t, toolCallHandler(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "from escalation"}))
	escalation := tierFor(escalationSrv, "escalation-model")
	m, err := New(Config{Primary: tierFor(primarySrv, "test-model"), Escalation: &escalation}, []string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]*router.OpenTaskSnapshot{"no open task": nil, "open task": {TaskID: "task-1", WorkspaceID: "ws-1"}} {
		t.Run(name, func(t *testing.T) {
			*primaryCount, *escalationCount = 0, 0
			dec, err := m.Decide(context.Background(), "a new machine please", nil, []router.TargetSnapshot{{ID: "t-1"}},
				func(o *router.DispatchOptions) { o.OpenTask = open })
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if dec.Action != router.ActionAnswerDirectly || dec.DirectAnswer != router.NoMachinePluginReply {
				t.Errorf("Decide = %+v, want the no-plugin answer", dec)
			}
			if dec.LeaveOpenTask != (open != nil) {
				t.Errorf("LeaveOpenTask = %v with open task %v", dec.LeaveOpenTask, open)
			}
			if *primaryCount != 1 || *escalationCount != 0 {
				t.Errorf("primary called %d times, escalation %d; want 1 and 0", *primaryCount, *escalationCount)
			}
		})
	}
}
