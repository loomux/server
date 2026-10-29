package router

import "testing"

func TestIsBareAffirmative(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    bool
	}{
		{"yes", true},
		{"Yes.", true},
		{"ok", true},
		{"go", true},
		{"do it", true},
		{"Sure, go ahead!", true},
		{"yep, do it please", true},
		{"go for it", true},
		{"let’s do it", true},
		{"👍", true},
		{"", false},
		{"it", false},
		{"please", false},
		{"no", false},
		{"yes, in workspace api", false},
		{"go fix the tests", false},
		{"do the task", false},
		{"yes?", false},
	} {
		if got := isBareAffirmative(tc.message); got != tc.want {
			t.Errorf("isBareAffirmative(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}
