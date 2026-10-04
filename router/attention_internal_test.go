package router

import (
	"slices"
	"testing"

	"github.com/Loomux/server/registry"
)

func TestPlanAnswer_Question(t *testing.T) {
	q := &registry.Attention{Kind: registry.AttentionQuestion, Question: "Red or blue?",
		Options: []registry.AttentionOption{{Label: "Red"}, {Label: "Blue"}, {Label: "Type something."}, {Label: "Chat about this"}}}
	cases := []struct {
		message string
		want    []keyStep
		problem bool
	}{
		{"2", []keyStep{{key: "Down"}, {key: "Enter"}}, false},
		{"green, actually", []keyStep{{key: "Down"}, {key: "Down"}, {text: "green, actually"}, {key: "Enter"}}, false},
		{"3", nil, true},
		{"deny", []keyStep{{key: "Escape"}}, false},
		{"yes", nil, true},
	}
	for _, tc := range cases {
		plan, problem := planAnswer(q, parseAnswer(tc.message))
		if (problem != "") != tc.problem || !slices.Equal(plan.steps, tc.want) {
			t.Errorf("%q: steps %v, problem %q; want %v (problem: %v)", tc.message, plan.steps, problem, tc.want, tc.problem)
		}
	}
}

func TestPlanAnswer_TrustAndCursor(t *testing.T) {
	trust := &registry.Attention{Kind: registry.AttentionTrust, Selected: 0,
		Options: []registry.AttentionOption{{Label: "No, exit"}, {Label: "Yes, I trust this folder"}}}
	if plan, _ := planAnswer(trust, parseAnswer("approve")); !slices.Equal(plan.steps, []keyStep{{key: "Down"}, {key: "Enter"}}) {
		t.Errorf("approve steps = %v", plan.steps)
	}
	if _, problem := planAnswer(trust, parseAnswer("why?")); problem == "" {
		t.Error("words to a trust dialog: want a problem")
	}
	// The cursor starting below the target moves up.
	perm := &registry.Attention{Kind: registry.AttentionPermission, Selected: 2,
		Options: []registry.AttentionOption{{Label: "Yes"}, {Label: "Yes, always"}, {Label: "No"}}}
	if plan, _ := planAnswer(perm, parseAnswer("ok")); !slices.Equal(plan.steps, []keyStep{{key: "Up"}, {key: "Up"}, {key: "Enter"}}) {
		t.Errorf("approve steps = %v", plan.steps)
	}
}
