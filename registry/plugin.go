package registry

import (
	"encoding/json"
	"time"
)

// Plugins installed on this server (LOOM-178, docs/design/target-providers.md
// §1): a row per installed instance of a plugin, with its configuration.

// PluginStatus is where an installed plugin is in its life.
type PluginStatus string

const (
	// PluginStatusInstalling: the row exists, the plugin hasn't passed its
	// first check yet.
	PluginStatusInstalling PluginStatus = "installing"
	// PluginStatusInstalled: it started, took its configuration and its
	// check was ok.
	PluginStatusInstalled PluginStatus = "installed"
	// PluginStatusDisabled: stopped on purpose; its row and configuration
	// are kept.
	PluginStatusDisabled PluginStatus = "disabled"
	// PluginStatusError: it couldn't start, or its check failed;
	// StatusReason says why.
	PluginStatusError PluginStatus = "error"
)

// PluginSource is where a plugin was found.
type PluginSource string

const (
	// PluginSourceBundled: in the image's bundle directory.
	PluginSourceBundled PluginSource = "bundled"
	// PluginSourceDir: in the operator's plugin directory.
	PluginSourceDir PluginSource = "dir"
	// PluginSourceSocket: a sidecar serving a unix socket.
	PluginSourceSocket PluginSource = "socket"
)

// Plugin trust values (Plugin.Trust).
const (
	// PluginTrustBundled: part of the image, trusted as the image is.
	PluginTrustBundled = "bundled"
	// PluginTrustUnsigned: nothing vouches for it; the user accepted it.
	PluginTrustUnsigned = "unsigned"
	// PluginTrustSignedPrefix starts a trust value naming the identity
	// whose signature verified: "signed:<identity>".
	PluginTrustSignedPrefix = "signed:"
	// PluginTrustPinnedPrefix starts a trust value naming the digest the
	// operator pinned the executable to: "pinned:sha256:<hex>" (LOOM-218).
	PluginTrustPinnedPrefix = "pinned:"
)

// Plugin is one installed plugin instance.
type Plugin struct {
	ID string
	// Name is the manifest's name ("kubernetes"); Label the instance's,
	// unique ("wysekube").
	Name  string
	Label string
	// Version and Protocol are the installed manifest's.
	Version  string
	Protocol string
	Source   PluginSource
	// Path is the plugin's directory, or its socket.
	Path string
	// Trust is one of the PluginTrust* values.
	Trust        string
	Status       PluginStatus
	StatusReason string
	Enabled      bool
	// Capabilities are the manifest's, as installed; an upgrade compares.
	Capabilities []string
	// Manifest is the installed manifest as JSON, so the configuration
	// form and an upgrade's comparison work without the plugin's files.
	Manifest json.RawMessage
	// Config is the configuration without its secret fields; Secrets are
	// those, plaintext at this level and encrypted at rest. Only
	// GetPlugin fills Secrets in; ListPlugins never decrypts.
	Config      map[string]any
	Secrets     map[string]string
	InstalledAt time.Time
	UpdatedAt   time.Time
}

// Settings keys (Store.GetSetting/SetSetting).
const (
	// SettingInstanceID is this server's instance id, generated on first
	// start: what labels everything a plugin makes as this server's.
	SettingInstanceID = "instance_id"
	// SettingPluginsSeedOwned is the ids of the plugin instances
	// LOOMUX_PLUGINS_SEED made, as a JSON array: the seed reconfigures and
	// prunes those only (LOOM-219, plugins/seed.go).
	SettingPluginsSeedOwned = "plugins_seed_owned"
	// SettingMachinesSeedOwned is the target ids of the machines the
	// seed's "machines" made, as a JSON array: the seed keeps those in
	// line only (LOOM-236, providers/seed.go).
	SettingMachinesSeedOwned = "machines_seed_owned"
)
