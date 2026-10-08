package targets

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// SSHFailure is why ssh couldn't reach a target (LOOM-85): a stable,
// low-cardinality class for errors, logs and metrics, so a changed host
// key, a dead SOCKS sidecar and a refused key don't all read as the same
// "unreachable".
type SSHFailure string

const (
	SSHHostKeyChanged    SSHFailure = "host_key_changed"
	SSHHostKeyUnknown    SSHFailure = "host_key_unknown"
	SSHAuthFailed        SSHFailure = "auth_failed"
	SSHDNSFailed         SSHFailure = "dns_failed"
	SSHProxyUnreachable  SSHFailure = "proxy_unreachable"
	SSHConnectionRefused SSHFailure = "connection_refused"
	SSHHostUnreachable   SSHFailure = "host_unreachable"
	SSHMuxSessionRefused SSHFailure = "mux_session_refused"
	SSHTimeout           SSHFailure = "timeout"
	SSHOther             SSHFailure = "other"
)

// UnreachableError is the ErrUnreachable a remote operation returns:
// errors.Is(err, ErrUnreachable) holds, and errors.As gives the class.
type UnreachableError struct {
	// Host is the target's ssh host (its name in the ssh config).
	Host    string
	Failure SSHFailure
	// Detail is what ssh said, its banner lines removed; or, for a
	// deadline Loomux enforced, what that deadline was.
	Detail string
	// Managed is set for a managed target (LOOM-138), which has no SSH
	// secret or config behind it: the hint mustn't point there.
	Managed bool
}

func (e *UnreachableError) Is(target error) bool { return target == ErrUnreachable }

func (e *UnreachableError) Error() string {
	msg := ErrUnreachable.Error() + ": " + e.Hint()
	if e.Detail != "" {
		msg += " (" + string(e.Failure) + ": " + e.Detail + ")"
	} else {
		msg += " (" + string(e.Failure) + ")"
	}
	return msg
}

// Hint says, in plain words, what went wrong and what to do about it.
func (e *UnreachableError) Hint() string {
	h := e.Host
	if e.Managed {
		switch e.Failure {
		case SSHHostKeyChanged:
			return "the host key of " + h + " has changed: if that's expected (the machine was reinstalled), scan and pin its new key " +
				"(Targets, or POST /api/v1/targets/{id}/scan-host-key then /pin); if not, find out why first"
		case SSHHostKeyUnknown:
			return h + "'s host key isn't pinned: scan and pin it (Targets, or POST /api/v1/targets/{id}/scan-host-key then /pin)"
		case SSHAuthFailed:
			return h + " refused Loomux's SSH key for this target: add its public key (Targets, or GET /api/v1/ssh-keys) to the target user's ~/.ssh/authorized_keys"
		case SSHDNSFailed:
			return "the host name " + h + " doesn't resolve: check the target's host"
		}
	}
	switch e.Failure {
	case SSHHostKeyChanged:
		return "the host key of " + h + " has changed: if that's expected (the machine was reinstalled), scan and pin its new key " +
			"(Targets, or POST /api/v1/targets/{id}/scan-host-key then /pin) or replace its known_hosts entry in the SSH secret; " +
			"if not, find out why first"
	case SSHHostKeyUnknown:
		return h + "'s host key isn't known: scan and pin it (Targets, or POST /api/v1/targets/{id}/scan-host-key then /pin), " +
			"or add it to the SSH secret's known_hosts"
	case SSHAuthFailed:
		return h + " refused Loomux's SSH key: add the public key to the target user's authorized_keys"
	case SSHDNSFailed:
		return "the host name " + h + " doesn't resolve: check the target's host and the ssh config"
	case SSHProxyUnreachable:
		return "the connection to " + h + " closed before SSH started: either the SOCKS proxy (is the Tailscale sidecar running?) or the target's sshd refusing it (throttling, fail2ban)"
	case SSHConnectionRefused:
		return h + " refused the connection: is sshd running and listening on that port?"
	case SSHHostUnreachable:
		return h + " can't be reached on the network: is it powered on and online?"
	case SSHMuxSessionRefused:
		return h + " refused another session on the shared SSH connection (sshd's MaxSessions?)"
	case SSHTimeout:
		return h + " didn't answer in time"
	default:
		return "could not connect to " + h
	}
}

// AsUnreachable returns err's UnreachableError, if it has one.
func AsUnreachable(err error) (*UnreachableError, bool) {
	var u *UnreachableError
	ok := errors.As(err, &u)
	return u, ok
}

// sshBanner matches stderr lines that are never the reason a connection
// failed: OpenSSH's post-quantum key exchange warning (three "** " lines
// on every new connection to an older sshd) and known_hosts notices.
var sshBanner = regexp.MustCompile(`^(\*\* |Warning: Permanently added |@+$|@ +WARNING: )`)

// stripSSHBanner drops the banner lines ssh writes to stderr before
// anything else, so they can't stand in for a command's error or be
// mixed into its output (LOOM-85: sc1's post-quantum warning). Only
// leading lines go: a command's own output that happens to look like
// one is kept. It runs on every command's stderr, so a command whose own
// stderr starts with a "** " or "@@@" line loses those lines; nothing
// Loomux runs does.
func stripSSHBanner(stderr string) string {
	for stderr != "" {
		line, rest, _ := strings.Cut(stderr, "\n")
		if !sshBanner.MatchString(strings.TrimRight(line, "\r")) {
			break
		}
		stderr = rest
	}
	return stderr
}

// sshFailurePatterns are tried in order on ssh's stderr; the first match
// wins. The proxy ones come before "Connection refused", which a dead
// SOCKS proxy also produces.
var sshFailurePatterns = []struct {
	re      *regexp.Regexp
	failure SSHFailure
}{
	{regexp.MustCompile(`REMOTE HOST IDENTIFICATION HAS CHANGED|Host key for .* has changed`), SSHHostKeyChanged},
	{regexp.MustCompile(`No \S+ host key is known for|Host key verification failed`), SSHHostKeyUnknown},
	{regexp.MustCompile(`Permission denied \(|Too many authentication failures`), SSHAuthFailed},
	{regexp.MustCompile(`Could not resolve hostname|Name or service not known|Temporary failure in name resolution|nodename nor servname`), SSHDNSFailed},
	// socat's SOCKS5-CONNECT failing to reach the proxy itself.
	{regexp.MustCompile(`socat\[\d+\] E .*(Connection refused|No such file)`), SSHProxyUnreachable},
	// OpenBSD nc reports the SOCKS server's own answer; 5 is the target
	// refusing, 3, 4 and 1 not being reachable through it.
	{regexp.MustCompile(`SOCKS error 5\b`), SSHConnectionRefused},
	{regexp.MustCompile(`SOCKS error \d+`), SSHHostUnreachable},
	// The ProxyCommand ended before the SSH banner and said nothing
	// (OpenBSD nc that can't reach its proxy is silent).
	{regexp.MustCompile(`Connection closed by UNKNOWN port 65535`), SSHProxyUnreachable},
	{regexp.MustCompile(`Session open refused by peer|mux_client_request_session: session request failed|administratively prohibited`), SSHMuxSessionRefused},
	{regexp.MustCompile(`Connection refused`), SSHConnectionRefused},
	{regexp.MustCompile(`No route to host|Network is unreachable|Host is unreachable`), SSHHostUnreachable},
	{regexp.MustCompile(`timed out|Operation timed out`), SSHTimeout},
}

// classifySSHFailure maps ssh's stderr from a failed connection (exit
// 255) to its class, and returns the stderr with banner lines removed.
func classifySSHFailure(stderr string) (SSHFailure, string) {
	var kept []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || sshBanner.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	all := strings.Join(kept, "\n")
	// ssh's reason is at the end; a host key warning before it runs to a
	// dozen lines.
	if len(kept) > maxDetailLines {
		kept = kept[len(kept)-maxDetailLines:]
	}
	detail := strings.Join(kept, "; ")
	if len(detail) > maxDetailBytes {
		cut := maxDetailBytes
		for cut > 0 && !utf8.RuneStart(detail[cut]) {
			cut--
		}
		detail = detail[:cut] + "…"
	}
	for _, p := range sshFailurePatterns {
		if p.re.MatchString(all) {
			return p.failure, detail
		}
	}
	return SSHOther, detail
}

const (
	maxDetailLines = 3
	maxDetailBytes = 400
)
