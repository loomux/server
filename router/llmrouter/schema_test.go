package llmrouter

import (
	"reflect"
	"regexp"
	"testing"
)

// targetIDProperty digs new_workspace.properties.target_id out of the
// built tool schema.
func targetIDProperty(t *testing.T, targetIDs []string) map[string]any {
	t.Helper()
	tool := buildDecideTool([]string{"claude-code"}, []string{"ws-1"}, targetIDs, false)
	params := map[string]any(tool.OfFunction.Function.Parameters)
	properties, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("tool parameters have no properties map: %+v", params)
	}
	newWorkspace, ok := properties["new_workspace"].(map[string]any)
	if !ok {
		t.Fatalf("properties have no new_workspace object: %+v", properties)
	}
	nested, ok := newWorkspace["properties"].(map[string]any)
	if !ok {
		t.Fatalf("new_workspace has no properties map: %+v", newWorkspace)
	}
	prop, ok := nested["target_id"].(map[string]any)
	if !ok {
		t.Fatalf("new_workspace.properties has no target_id: %+v", nested)
	}
	return prop
}

// TestBuildDecideTool_TargetIDEnumConstrained proves target_id is
// enum-constrained to the registered target IDs exactly as agent_type and
// workspace_id already are (LOOM-64) — the schema-level half of stopping
// the model from inventing a target.
func TestBuildDecideTool_TargetIDEnumConstrained(t *testing.T) {
	prop := targetIDProperty(t, []string{"target-1", "target-2"})
	got, ok := prop["enum"].([]string)
	if !ok {
		t.Fatalf("target_id has no []string enum: %+v", prop)
	}
	want := []string{"target-1", "target-2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("target_id enum = %v, want %v", got, want)
	}
}

// TestBuildDecideTool_NoTargets_TargetIDUnconstrained matches
// enumStringProperty's existing contract: an empty value set adds no
// enum key at all (an empty JSON Schema enum matches nothing and some
// providers reject it).
func TestBuildDecideTool_NoTargets_TargetIDUnconstrained(t *testing.T) {
	prop := targetIDProperty(t, nil)
	if _, ok := prop["enum"]; ok {
		t.Errorf("target_id carries an enum with no registered targets: %+v", prop)
	}
}

// actionEnum returns the action enum the decide tool offers the model.
func actionEnum(t *testing.T, targetIDs []string) []string {
	t.Helper()
	tool := buildDecideTool([]string{"claude-code"}, nil, targetIDs, false)
	params := tool.OfFunction.Function.Parameters
	props, _ := params["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	enum, ok := action["enum"].([]string)
	if !ok {
		t.Fatalf("action enum missing or not []string: %#v", action["enum"])
	}
	return enum
}

// TestBuildDecideTool_NoTargets_HidesTargetActions (LOOM-68): with no
// registered target there is no machine to provision on or run a command
// on, so neither action is offered; once a target exists both are.
func TestBuildDecideTool_NoTargets_HidesTargetActions(t *testing.T) {
	if got, want := actionEnum(t, nil), []string{"answer_directly", "use_workspace"}; !reflect.DeepEqual(got, want) {
		t.Errorf("no targets: action enum = %v, want %v", got, want)
	}
	if got, want := actionEnum(t, []string{"target-1"}),
		[]string{"answer_directly", "use_workspace", "provision_workspace", "run_command"}; !reflect.DeepEqual(got, want) {
		t.Errorf("one target: action enum = %v, want %v", got, want)
	}
}

// TestBuildDecideTool_WorkspaceNamePatternAcceptsUnderscore proves the
// schema's name pattern is the provisioner's: a name the provisioner
// accepts (underscores included) must not be rejected by the schema.
func TestBuildDecideTool_WorkspaceNamePatternAcceptsUnderscore(t *testing.T) {
	tool := buildDecideTool([]string{"claude-code"}, []string{"ws-1"}, []string{"target-1"}, false)
	params := map[string]any(tool.OfFunction.Function.Parameters)
	nested := params["properties"].(map[string]any)["new_workspace"].(map[string]any)["properties"].(map[string]any)
	pattern, ok := nested["name"].(map[string]any)["pattern"].(string)
	if !ok {
		t.Fatalf("new_workspace.name has no string pattern: %+v", nested["name"])
	}
	re := regexp.MustCompile(pattern)
	for _, name := range []string{"my_ws", "web-app", "a1"} {
		if !re.MatchString(name) {
			t.Errorf("schema pattern %q rejects %q", pattern, name)
		}
	}
	for _, name := range []string{"_ws", "Web", "a/b"} {
		if re.MatchString(name) {
			t.Errorf("schema pattern %q accepts %q", pattern, name)
		}
	}
}
