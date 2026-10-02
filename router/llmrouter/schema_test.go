package llmrouter

import (
	"reflect"
	"testing"
)

// targetIDProperty digs new_workspace.properties.target_id out of the
// built tool schema.
func targetIDProperty(t *testing.T, targetIDs []string) map[string]any {
	t.Helper()
	tool := buildDecideTool([]string{"claude-code"}, []string{"ws-1"}, targetIDs)
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
