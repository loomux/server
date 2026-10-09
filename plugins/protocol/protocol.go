// Package protocol is the loomux-plugin/1 protocol's vocabulary: the
// method names the host calls and the parameter and result types they
// carry (docs/design/target-providers.md §1.3). It imports nothing of
// Loomux, so the host, the SDK and any plugin share it without cycles.
package protocol

// Version is the protocol this package describes.
const Version = "loomux-plugin/1"

// Methods of the plugin.* group, which every plugin serves.
const (
	// MethodDescribe returns the plugin's manifest: the handshake.
	MethodDescribe = "plugin.describe"
	// MethodConfigure hands the plugin its configuration and what it
	// needs to know about the host (ConfigureParams).
	MethodConfigure = "plugin.configure"
	// MethodCheck asks the plugin to verify its credentials and
	// reachability (CheckResult).
	MethodCheck = "plugin.check"
	// MethodShutdown asks it to stop cleanly.
	MethodShutdown = "plugin.shutdown"
)

// NotifyTargetsChanged is the one notification a plugin may send the
// host: a hint that a machine's status changed, never the truth.
const NotifyTargetsChanged = "targets.changed"

// HostInfo is what a plugin learns about the host at configure.
type HostInfo struct {
	// Version is the host's version string.
	Version string `json:"version"`
	// InstanceID labels everything the plugin makes as this server's.
	InstanceID string `json:"instance_id"`
	// DataDir is a directory of the plugin's own on the host's data
	// volume, for a subprocess that needs to keep state.
	DataDir string `json:"data_dir,omitempty"`
}

// ConfigureParams are plugin.configure's parameters.
type ConfigureParams struct {
	// Config is the configuration as the manifest's schema describes it,
	// secret fields included: the channel is private to the two
	// processes.
	Config map[string]any `json:"config"`
	Host   HostInfo       `json:"host"`
}

// Problem severities.
const (
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// Problem is one thing plugin.check found.
type Problem struct {
	// Code is stable and machine-readable ("network_policy_not_enforced");
	// Message is for a person.
	Code     string `json:"code"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// CheckResult is plugin.check's result. OK is false when any problem has
// severity error.
type CheckResult struct {
	OK       bool      `json:"ok"`
	Problems []Problem `json:"problems"`
}

// TargetsChanged is NotifyTargetsChanged's parameters.
type TargetsChanged struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
