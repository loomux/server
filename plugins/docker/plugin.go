// Package docker is the Docker target-provider plugin (LOOM-180,
// docs/design/target-providers.md §9): one container per machine on
// one Docker host, reached over SSH (docker system dial-stdio) or the
// engine's local socket, with a named volume for sshd's files (the
// machine's record) and one for a persistent machine's data. Its
// executable is cmd/loomux-plugin-docker; the module is its own so the
// SSH and SOCKS libraries never enter the server's dependency graph.
package docker

import (
	"context"
	_ "embed"

	"github.com/Loomux/server/plugins"
)

// ManifestJSON is the plugin's plugin.json, the one its executable
// serves at describe.
//
//go:embed plugin.json
var ManifestJSON []byte

// Version is the plugin's, as plugin.json says.
const Version = "0.1.0"

// Plugin is the Docker plugin.
type Plugin struct{}

// New returns the plugin for real use.
func New() *Plugin { return &Plugin{} }

func (p *Plugin) Describe(ctx context.Context) (*plugins.Manifest, error) {
	return plugins.ParseManifest(ManifestJSON)
}
