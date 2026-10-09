package registry

import "time"

// Environments: machines a plugin made (LOOM-178,
// docs/design/target-providers.md §1.7). One per target; the target row
// is an ordinary managed remote target, this row is what the plugin
// knows about it.

// EnvironmentStatus is where a machine is in its life.
type EnvironmentStatus string

const (
	EnvironmentCreating   EnvironmentStatus = "creating"
	EnvironmentStarting   EnvironmentStatus = "starting"
	EnvironmentRunning    EnvironmentStatus = "running"
	EnvironmentStopped    EnvironmentStatus = "stopped"
	EnvironmentRecreating EnvironmentStatus = "recreating"
	EnvironmentDestroying EnvironmentStatus = "destroying"
	// EnvironmentLost: the plugin no longer has the machine and its data
	// is gone (an ephemeral one evicted).
	EnvironmentLost EnvironmentStatus = "lost"
	// EnvironmentError: creation or an operation failed; StatusReason
	// says why, the plugin's objects are left for inspection.
	EnvironmentError EnvironmentStatus = "error"
	// EnvironmentDetached: its plugin was uninstalled keeping the
	// machines; nothing manages it any more.
	EnvironmentDetached EnvironmentStatus = "detached"
)

// Environment is one machine a plugin made for a target.
type Environment struct {
	// ID is the short id every object the plugin makes is named from
	// (lx-<id>).
	ID       string
	TargetID string
	// PluginID is the installed plugin instance that owns the machine;
	// empty once it was uninstalled keeping its machines. PluginName
	// and PluginVersion stay, for the "created by" badge.
	PluginID      string
	PluginName    string
	PluginVersion string
	Status        EnvironmentStatus
	StatusReason  string
	Size          string
	Persistent    bool
	Egress        string
	// Image is the reference the machine was created (or recreated)
	// with; ImageDigest what runs, once known.
	Image       string
	ImageDigest string
	// HostKey is the machine's host public key (the pin's source);
	// HostPrivateKey its private half, plaintext at this level and
	// encrypted at rest, only filled in by GetEnvironment.
	HostKey        string
	HostPrivateKey []byte
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
