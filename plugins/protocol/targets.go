package protocol

import (
	"regexp"
	"time"
)

// The targets.* group: a target provider makes machines on demand
// (docs/design/target-providers.md §1.3, §1.9). The host calls these
// only on a plugin whose manifest declares targets.create.
const (
	MethodTargetsDescribe       = "targets.describe"
	MethodTargetsCreate         = "targets.create"
	MethodTargetsGet            = "targets.get"
	MethodTargetsList           = "targets.list"
	MethodTargetsStart          = "targets.start"
	MethodTargetsStop           = "targets.stop"
	MethodTargetsRecreate       = "targets.recreate"
	MethodTargetsDestroy        = "targets.destroy"
	MethodTargetsHealth         = "targets.health"
	MethodTargetsAttachCommands = "targets.attach_commands"
)

// Capabilities of the targets group, as a manifest declares them.
const (
	CapTargetsCreate         = "targets.create"
	CapTargetsStopStart      = "targets.stop_start"
	CapTargetsRecreate       = "targets.recreate"
	CapTargetsPersistent     = "targets.persistent"
	CapTargetsEphemeral      = "targets.ephemeral"
	CapTargetsEgressPolicy   = "targets.egress_policy"
	CapTargetsAttachCommands = "targets.attach_commands"
)

// Environment statuses (Environment.Status).
const (
	EnvCreating   = "creating"
	EnvStarting   = "starting"
	EnvRunning    = "running"
	EnvStopped    = "stopped"
	EnvRecreating = "recreating"
	EnvDestroying = "destroying"
	EnvLost       = "lost"
	EnvError      = "error"
)

// EnvironmentStatuses is every status a plugin may report.
var EnvironmentStatuses = []string{EnvCreating, EnvStarting, EnvRunning, EnvStopped, EnvRecreating, EnvDestroying, EnvLost, EnvError}

// Labels the host puts on every spec, and a plugin on every object it
// makes, so it only ever touches its own.
const (
	LabelInstance    = "loomux.io/instance"
	LabelTarget      = "loomux.io/target"
	LabelEnvironment = "loomux.io/environment"
)

// Egress values. EgressTrusted (LOOM-226) is the opt-in class for the
// machines an admin designates: the LAN and the tailnet as well as the
// internet. A plugin lists it last, if at all, and the host never picks
// it unasked.
const (
	EgressInternet = "internet"
	EgressNone     = "none"
	EgressTrusted  = "trusted"
)

// SSH proxy values (TargetsInfo.SSHProxy, Address.Proxy): how the host
// reaches the machines, as a target's ssh_proxy.
const (
	SSHProxyNone    = "none"
	SSHProxyDefault = "default"
)

// Size is one machine size the plugin offers.
type Size struct {
	Name   string `json:"name"`
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Disk   string `json:"disk"`
}

// TargetsInfo is targets.describe's result: what the plugin offers
// right now.
type TargetsInfo struct {
	Sizes             []Size   `json:"sizes"`
	PersistentDefault bool     `json:"persistent_default"`
	EgressOptions     []string `json:"egress_options"`
	Image             string   `json:"image"`
	MaxEnvironments   int      `json:"max_environments"`
	Environments      int      `json:"environments"`
	// AddressTemplate is where a machine will be reachable, with {id}
	// for the environment id: the host registers the target before the
	// machine exists.
	AddressTemplate string `json:"address_template"`
	// SSHProxy is how the host reaches the machines: none (directly) or
	// default (through LOOMUX_SSH_PROXY).
	SSHProxy string `json:"ssh_proxy"`
	// User and Port are sshd's in the machine.
	User string `json:"user"`
	Port int    `json:"port"`
}

// SSHBootstrap is what a machine needs to be reachable: the target's
// public key for authorized_keys, and the host key the host generated
// and pinned.
type SSHBootstrap struct {
	AuthorizedKey  string `json:"authorized_key"`
	HostPrivateKey string `json:"host_private_key"`
	HostPublicKey  string `json:"host_public_key"`
	Port           int    `json:"port"`
}

// EnvironmentSpec is targets.create's parameter: everything a plugin
// needs for one machine, and nothing else.
type EnvironmentSpec struct {
	ID         string            `json:"id"`
	TargetID   string            `json:"target_id"`
	Name       string            `json:"name"`
	Size       string            `json:"size"`
	Persistent bool              `json:"persistent"`
	Egress     string            `json:"egress"`
	Image      string            `json:"image"`
	SSH        SSHBootstrap      `json:"ssh"`
	Labels     map[string]string `json:"labels"`
}

// Address is how the host reaches a machine's sshd.
type Address struct {
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Proxy string `json:"proxy"`
}

// Environment is a machine as the plugin sees it.
type Environment struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// Progress, while the machine comes up, names a step that is
	// progressing rather than stuck, which the host waits out instead of
	// counting it against its deadline (LOOM-203): ProgressImagePull.
	// Empty otherwise. A host that doesn't know it ignores it.
	Progress    string    `json:"progress,omitempty"`
	Address     Address   `json:"address"`
	ImageDigest string    `json:"image_digest,omitempty"`
	Size        string    `json:"size"`
	Persistent  bool      `json:"persistent"`
	Egress      string    `json:"egress"`
	CreatedAt   time.Time `json:"created_at"`
}

// ProgressImagePull: the machine's image is being pulled, and nothing
// has failed yet (LOOM-203). A pull that fails is status error.
const ProgressImagePull = "image_pull"

// EnvironmentHealth is targets.health's result: the plugin's own view.
type EnvironmentHealth struct {
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	Restarts    int    `json:"restarts"`
	ImageDigest string `json:"image_digest,omitempty"`
}

// AttachCommand is one way a person attaches to a machine's tmux.
type AttachCommand struct {
	Via     string `json:"via"`
	Command string `json:"command"`
}

// Parameters of the id-taking methods.
type IDParams struct {
	ID string `json:"id"`
}

// RecreateParams are targets.recreate's.
type RecreateParams struct {
	ID   string          `json:"id"`
	Spec EnvironmentSpec `json:"spec"`
}

// AttachParams are targets.attach_commands'. Socket is the tmux socket
// the host runs the machine's sessions on, its instance's (LOOM-210):
// the commands must attach to that one. A host from before LOOM-210
// sends none, which means DefaultTmuxSocket.
type AttachParams struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Socket  string `json:"socket,omitempty"`
}

// DefaultTmuxSocket is the socket an AttachParams without one names:
// the host's own default (targets.DefaultTmuxSocket).
const DefaultTmuxSocket = "loomux"

var tmuxSocketPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidTmuxSocket reports whether name can be a socket name, the
// grammar targets.ValidTmuxSocket enforces on the host: it goes into
// the shell commands a plugin returns, unquoted.
func ValidTmuxSocket(name string) bool { return tmuxSocketPattern.MatchString(name) }

// ListResult is targets.list's result.
type ListResult struct {
	Environments []Environment `json:"environments"`
}

// AttachResult is targets.attach_commands' result.
type AttachResult struct {
	Commands []AttachCommand `json:"commands"`
}

// RequiredCapability is the capability a manifest must declare for the
// host to call method; "" for a method outside the targets group.
func RequiredCapability(method string) string {
	switch method {
	case MethodTargetsDescribe, MethodTargetsCreate, MethodTargetsGet, MethodTargetsList, MethodTargetsDestroy, MethodTargetsHealth:
		return CapTargetsCreate
	case MethodTargetsStart, MethodTargetsStop:
		return CapTargetsStopStart
	case MethodTargetsRecreate:
		return CapTargetsRecreate
	case MethodTargetsAttachCommands:
		return CapTargetsAttachCommands
	}
	return ""
}
