package targets_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/sshtest"
)

// LOOM-114 review: a stored user or host starting with "-" is a
// destination to ssh, never an option. Without "--" before user@host,
// -oProxyCommand=… ran its command on the Loomux server.
func TestRemoteExecutor_DestinationIsNeverAnOption(t *testing.T) {
	server := sshtest.Start(t)
	ctx := context.Background()
	// ";#" ends the injected command before the "@host" ssh would append.
	// The host form needs no user in front of it, as ScanHostKey sends a
	// target without one.
	for name, inject := range map[string]func(marker string) (host, user string){
		"user": func(m string) (string, string) { return server.Host, "-oProxyCommand=touch " + m + ";#" },
		"host": func(m string) (string, string) { return "-oProxyCommand=touch " + m + ";#", "" },
	} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "pwned")
			host, user := inject(marker)
			e := targets.NewRemoteExecutor(host, user, targets.WithPort(server.Port),
				targets.WithIdentityFile(server.IdentityFile), targets.WithConnectTimeout("3"),
				targets.WithExtraSSHArgs("-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null"))
			t.Cleanup(func() { _ = e.Close() })
			_, _ = e.RunOnce(ctx, "true")
			if _, err := targets.ScanHostKey(ctx, &registry.Target{ID: "t", Kind: registry.TargetKindRemote, Host: host,
				User: user, SSHPort: server.Port}); err == nil {
				t.Errorf("ScanHostKey with an injected %s succeeded", name)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("the injected ProxyCommand ran on the Loomux side (%s)", name)
			}
		})
	}
}
