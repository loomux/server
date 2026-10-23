package plugins

import (
	"fmt"
	"strings"

	"github.com/Loomux/server/registry"
)

// Verification of plugins from the plugin directory (LOOM-218): the
// Verifier loomuxd gives the catalog is plugins/attest's; what the
// Manager does with its answer is here and in manager.go
// (RequireVerified).

// UnverifiedError refuses to install or upgrade a plugin from the plugin
// directory that nothing vouches for (LOOM-218).
type UnverifiedError struct {
	Plugin, Reason string
}

func (e *UnverifiedError) Error() string {
	return fmt.Sprintf("plugins: %s isn't verified: %s. Pin its digest (LOOMUX_PLUGIN_DIGESTS) or trust its signer (LOOMUX_PLUGIN_SIGNERS), or set LOOMUX_PLUGIN_ALLOW_UNSIGNED=true to run unverified plugins", e.Plugin, e.Reason)
}

// verifiedTrust is whether a trust value says something vouched for the
// plugin.
func verifiedTrust(trust string) bool {
	return strings.HasPrefix(trust, registry.PluginTrustSignedPrefix) || strings.HasPrefix(trust, registry.PluginTrustPinnedPrefix)
}
