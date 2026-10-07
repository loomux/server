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
		// Security (LOOM-114 review): user and host go into ssh's argv.
		{
			name:    "user that ssh would read as an option",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindRemote, Host: "box", User: "-oProxyCommand=touch /tmp/pwned"},
			wantErr: "user must be a login name: a letter or _, then letters, digits, _ . or -, at most 32",
		},
		{
			name:    "user with an @",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindRemote, Host: "box", User: "a@evil"},
			wantErr: "user must be a login name: a letter or _, then letters, digits, _ . or -, at most 32",
		},
		{
			name:    "host that ssh would read as an option",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindRemote, Host: "-oProxyCommand=touch /tmp/pwned", User: "u"},
			wantErr: "host must be a host name or address (no leading -, whitespace, @ or /)",
		},
		{
			name:    "host with whitespace",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindRemote, Host: "box -p 2222", User: "u"},
			wantErr: "host must be a host name or address (no leading -, whitespace, @ or /)",
		},
		{
			name:   "valid user and IPv6 host",
			target: registry.Target{Name: "x", Kind: registry.TargetKindRemote, Host: "fd7a:115c::1", User: "deploy_user-2.x"},
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
			name:   "absolute workspace root",
			target: registry.Target{Name: "x", Kind: registry.TargetKindLocal, WorkspaceRoot: "/srv/loomux"},
		},
		{
			name:    "relative workspace root",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindLocal, WorkspaceRoot: "work"},
			wantErr: "workspace_root must be an absolute path",
		},
		{
			name:    "tilde workspace root",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindLocal, WorkspaceRoot: "~/work"},
			wantErr: "workspace_root must be an absolute path",
		},
		{
			name:    "unclean workspace root",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindLocal, WorkspaceRoot: "/srv/../etc"},
			wantErr: "workspace_root must be a clean path (no ., .. or trailing /)",
		},
		{
			name:    "filesystem root as workspace root",
			target:  registry.Target{Name: "x", Kind: registry.TargetKindLocal, WorkspaceRoot: "/"},
			wantErr: "workspace_root must not be /",
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

func TestTargetValidate_PermissionMode(t *testing.T) {
	for mode, ok := range map[string]bool{"": true, "auto": true, "accept-edits": true, "manual": true, "bypass": false, "yolo": false} {
		target := &registry.Target{Name: "t", Kind: registry.TargetKindLocal, PermissionMode: mode}
		if err := target.Validate(); (err == nil) != ok {
			t.Errorf("Validate(permission_mode=%q) = %v, want ok=%v", mode, err, ok)
		}
	}
}
