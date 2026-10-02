package registry_test

import (
	"testing"

	"github.com/Loomux/server/registry"
)

// TestTargetValidate pins the target invariants at the registry level
// (LOOM-65), so every entry point — the HTTP API today, an admin CLI
// later — shares one validation path. The messages are caller-facing:
// the API returns them verbatim as 400 bodies.
func TestTargetValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  registry.Target
		wantErr string
	}{
		{
			name:   "valid local",
			target: registry.Target{Name: "jet01", Kind: registry.TargetKindLocal},
		},
		{
			name:   "valid remote",
			target: registry.Target{Name: "bigbox", Kind: registry.TargetKindRemote, Host: "bigbox.example.invalid", User: "loomux"},
		},
		{
			name:    "missing name",
			target:  registry.Target{Kind: registry.TargetKindLocal},
			wantErr: "name is required",
		},
		{
			name:    "whitespace name",
			target:  registry.Target{Name: "   ", Kind: registry.TargetKindLocal},
			wantErr: "name is required",
		},
		{
			name:    "unknown kind",
			target:  registry.Target{Name: "x", Kind: "carrier-pigeon"},
			wantErr: `kind must be "local" or "remote"`,
		},
		{
			name:    "empty kind",
			target:  registry.Target{Name: "x"},
			wantErr: `kind must be "local" or "remote"`,
		},
		{
			name:    "local with host",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindLocal, Host: "h"},
			wantErr: "host and user must be empty for a local target",
		},
		{
			name:    "local with user",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindLocal, User: "u"},
			wantErr: "host and user must be empty for a local target",
		},
		{
			name:    "remote without user",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindRemote, Host: "h"},
			wantErr: "host and user are required for a remote target",
		},
		{
			name:    "remote without host",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindRemote, User: "u"},
			wantErr: "host and user are required for a remote target",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.target.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("Validate() = %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}
