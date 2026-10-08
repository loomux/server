package router

import "testing"

// LOOM-152: a model-chosen remote skips the clone confirmation only when
// the user's message names it as a whole word.
func TestNamesRemote(t *testing.T) {
	const remote = "https://github.com/alice/tools"
	for msg, want := range map[string]bool{
		"set up https://github.com/alice/tools and look around":      true,
		"clone https://github.com/alice/tools.":                      true,
		"clone (https://github.com/alice/tools)":                     true,
		"clone `https://github.com/alice/tools`":                     true,
		`clone "https://github.com/alice/tools.git"`:                 true,
		"clone https://github.com/alice/tools/ please":               true,
		"set up https://github.com/alice/tools-fork and look around": false,
		"set up https://github.com/alice/toolsx":                     false,
		"use github.com/alice/tools":                                 false,
		"https://github.com/alice/tools/tree/main":                   false,
		"": false,
	} {
		if got := namesRemote(msg, remote); got != want {
			t.Errorf("namesRemote(%q) = %v, want %v", msg, got, want)
		}
	}
	if namesRemote("anything", "") {
		t.Error("an empty remote was named")
	}
}
