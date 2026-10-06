package router

import (
	"fmt"
	"reflect"
	"testing"
)

func offeredFixture(n int) []WorkspaceSnapshot {
	out := make([]WorkspaceSnapshot, n)
	for i := range out {
		out[i] = WorkspaceSnapshot{ID: fmt.Sprintf("ws-%02d", i), Name: fmt.Sprintf("project-%02d", i)}
	}
	return out
}

func ids(ws []WorkspaceSnapshot) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.ID
	}
	return out
}

// LOOM-107: a long workspace list is cut to the most recent, but the
// hinted, last-used, open-task and named workspaces always stay, in the
// original (recency) order.
func TestCapOffered(t *testing.T) {
	offered := offeredFixture(10) // most recent first
	offered[8].Name = "billing_api"

	got := ids(capOffered(offered, 5, "deploy the Billing API please", "ws-09", "ws-07", ""))
	// pinned: ws-09 (hint), ws-07 (last), ws-08 (named "billing api");
	// then the two most recent.
	want := []string{"ws-00", "ws-01", "ws-07", "ws-08", "ws-09"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("capOffered = %v, want %v", got, want)
	}

	if got := capOffered(offered, 20, "x"); len(got) != 10 {
		t.Errorf("under the cap: %d workspaces, want all 10", len(got))
	}

	// A pinned ID that isn't offered (an archived workspace's hint) takes
	// no slot.
	got = ids(capOffered(offered, 3, "x", "ws-gone"))
	if want := []string{"ws-00", "ws-01", "ws-02"}; !reflect.DeepEqual(got, want) {
		t.Errorf("capOffered with an unknown pin = %v, want %v", got, want)
	}

	// More pinned than the cap: all pinned stay, nothing else is added.
	got = ids(capOffered(offered, 2, "x", "ws-05", "ws-06", "ws-07"))
	if want := []string{"ws-05", "ws-06", "ws-07"}; !reflect.DeepEqual(got, want) {
		t.Errorf("over-pinned capOffered = %v, want %v", got, want)
	}
}

func TestMentions(t *testing.T) {
	for _, tc := range []struct {
		message, name string
		want          bool
	}{
		{"fix the loomux-web build", "loomux-web", true},
		{"fix the loomux web build", "loomux-web", true},
		{"look at jet01 connectivity test", "jet01_connectivity_test", true},
		{"go", "go", false}, // too short to trust
		{"nothing here", "loomux-web", false},
	} {
		if got := mentions(tc.message, tc.name); got != tc.want {
			t.Errorf("mentions(%q, %q) = %v, want %v", tc.message, tc.name, got, tc.want)
		}
	}
}
