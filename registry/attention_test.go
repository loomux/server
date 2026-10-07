package registry_test

import (
	"encoding/json"
	"testing"

	"github.com/Loomux/server/registry"
)

// The usage-limit reset text is "resets" since the API v1 freeze review
// (item 11); an attention stored before, under "resets_at", still reads.
func TestAttentionResetsKey(t *testing.T) {
	var old registry.Attention
	if err := json.Unmarshal([]byte(`{"kind":"usage_limit","resets_at":"5pm (Europe/Istanbul)","selected":0}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Kind != registry.AttentionUsageLimit || old.Resets != "5pm (Europe/Istanbul)" {
		t.Fatalf("stored attention = %+v, want its reset text kept", old)
	}
	var both registry.Attention
	if err := json.Unmarshal([]byte(`{"kind":"usage_limit","resets":"new","resets_at":"old"}`), &both); err != nil {
		t.Fatal(err)
	}
	if both.Resets != "new" {
		t.Fatalf("resets = %q, want the new key to win", both.Resets)
	}
	out, _ := json.Marshal(registry.Attention{Kind: registry.AttentionUsageLimit, Resets: "in 2 hours"})
	if string(out) != `{"kind":"usage_limit","selected":0,"resets":"in 2 hours"}` {
		t.Fatalf("marshalled = %s", out)
	}
}
