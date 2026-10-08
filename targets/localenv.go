package targets

import (
	"errors"
	"os"
	"strings"
)

// LocalTargets says whether local targets may run at all (LOOM-141). A
// local target's agents run as loomuxd's own user on loomuxd's own
// machine: whatever they run can read loomuxd's database (and with it
// the vault), its SSH keys and its process environment. That's only
// acceptable where the person running loomuxd means it, so the
// container image turns it off (LOOMUX_LOCAL_TARGETS=off) and a bare
// binary leaves it on. Set once at startup, with SetLocalTargets.
var LocalTargets = true

// SetLocalTargets sets LocalTargets. Call it once at startup.
func SetLocalTargets(on bool) { LocalTargets = on }

// ErrLocalTargetsOff is what running anything on a local target returns
// while local targets are turned off.
var ErrLocalTargetsOff = errors.New("local targets are turned off on this server (LOOMUX_LOCAL_TARGETS=off): their agents would run as the server's own user, with access to its database and keys")

// localEnvAllowed are the variables a local target's processes inherit
// from loomuxd. Everything else, LOOMUX_* secrets above all, stays with
// loomuxd. This is defence in depth only: a same-user process can still
// read loomuxd's environment from /proc and its database file, which is
// why a local target is fully trusted.
var localEnvAllowed = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TERM": true, "LANG": true, "TZ": true, "TMPDIR": true, "TMUX_TMPDIR": true,
	"XDG_RUNTIME_DIR": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true,
}

// localEnv is loomuxd's environment cut down to localEnvAllowed (plus
// the LC_* locale variables).
func localEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if localEnvAllowed[name] || strings.HasPrefix(name, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}
