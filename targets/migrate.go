package targets

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/Loomux/server/registry"
)

// Migrating a config-mode target to a managed one (LOOM-138, PR 3):
// what the mounted SSH config does for it — the real host behind an
// alias, the port, the key file, the proxy — is read from `ssh -G`, the
// host key from the known_hosts files ssh would check, and the key file
// is imported. Nothing is changed here; the caller applies the plan.

// sshConfigFile, when set, is the config ResolveSSHConfig reads (ssh -F)
// instead of the user's own, which every config-mode connection uses.
var sshConfigFile = ""

// sshDir is the mounted ~/.ssh: the only place a key is imported from.
var sshDir = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh"), nil
}

// SSHConfigPlan is a config-mode target as a managed one would reach it.
type SSHConfigPlan struct {
	Host string
	Port int
	User string
	// SSHProxy is "" (the server's LOOMUX_SSH_PROXY, which the config's
	// proxy command matches) or registry.SSHProxyNone.
	SSHProxy string
	// KeyFile is the key file the config uses; Key is it, imported (no
	// ID yet), when it could be.
	KeyFile string
	Key     *registry.SSHKey
	// HostKeys are the known_hosts lines to pin, keyed by Host:Port.
	HostKeys string
	// Problems are what keeps the target from being migrated as is,
	// worded for a person.
	Problems []string
}

const (
	resolveTimeout = 10 * time.Second
	// maxKeyFileSize bounds an imported key file; an SSH private key is a
	// few KiB at most.
	maxKeyFileSize = 16 << 10
)

// socksProxyCommands are the SOCKS5 proxy commands a config may use that
// Loomux's own relay replaces; the submatch is the proxy's host:port.
var socksProxyCommands = []*regexp.Regexp{
	regexp.MustCompile(`^(?:/usr/bin/|/bin/)?n(?:c|cat) -X 5 -x (\S+) %h %p$`),
	regexp.MustCompile(`^(?:/usr/bin/)?socat - SOCKS5-CONNECT:(\S+):%h:%p$`),
}

// ResolveSSHConfig reads what the SSH config does for t, a config-mode
// remote target, and plans it as a managed one.
func ResolveSSHConfig(ctx context.Context, t *registry.Target) (*SSHConfigPlan, error) {
	if t.Kind != registry.TargetKindRemote || t.Managed() {
		return nil, errors.New("targets: only a remote target reached through the SSH config can be migrated")
	}
	cfg, err := sshConfigFor(ctx, t)
	if err != nil {
		return nil, err
	}
	plan := &SSHConfigPlan{Host: first(cfg["hostname"]), User: first(cfg["user"])}
	plan.Port, _ = strconv.Atoi(first(cfg["port"]))
	problem := func(format string, args ...any) { plan.Problems = append(plan.Problems, fmt.Sprintf(format, args...)) }

	candidate := registry.Target{Name: t.Name, Kind: registry.TargetKindRemote, Host: plan.Host, User: plan.User, SSHKeyRef: "migrating", SSHPort: plan.Port}
	if err := candidate.Validate(); err != nil {
		problem("the SSH config's host %q for this target can't be used: %v", plan.Host, err)
	}
	if jump := first(cfg["proxyjump"]); jump != "" && jump != "none" {
		problem("the SSH config reaches it through ProxyJump %s; managed targets don't support jump hosts", jump)
	}
	plan.SSHProxy = registry.SSHProxyNone
	if pc := first(cfg["proxycommand"]); pc != "" && pc != "none" {
		addr := socksProxyAddr(pc)
		m := currentManaged()
		switch {
		case addr == "":
			problem("the SSH config's ProxyCommand %q isn't a SOCKS5 proxy Loomux can relay", pc)
		case m == nil || m.Proxy != addr:
			server := ""
			if m != nil {
				server = m.Proxy
			}
			problem("the SSH config goes through the SOCKS5 proxy %s, but LOOMUX_SSH_PROXY is %q; set it to socks5://%s", addr, server, addr)
		default:
			plan.SSHProxy = ""
		}
	}

	home, ssh := "", ""
	if dir, err := sshDir(); err == nil {
		ssh, home = filepath.Clean(dir), filepath.Dir(filepath.Clean(dir))
	}
	plan.KeyFile, plan.Key, err = importIdentity(cfg["identityfile"], home, ssh)
	if err != nil {
		problem("%v", err)
	}

	addr := knownhosts.Normalize(net.JoinHostPort(plan.Host, strconv.Itoa(plan.Port)))
	if strings.TrimSpace(t.HostKeys) != "" {
		plan.HostKeys, err = rekeyHostKeys(t.HostKeys, addr)
	} else {
		plan.HostKeys, err = lookupKnownHosts(ctx, cfg, plan, home, addr)
	}
	if err != nil {
		problem("%v", err)
	} else if plan.HostKeys == "" {
		problem("no host key for %s in the target's pin or the SSH config's known_hosts: scan and pin it first", addr)
	}
	return plan, nil
}

// sshConfigFor is `ssh -G` for t: the settings ssh would use, one
// "keyword value" per line.
func sshConfigFor(ctx context.Context, t *registry.Target) (map[string][]string, error) {
	var args []string
	if sshConfigFile != "" {
		args = append(args, "-F", sshConfigFile)
	}
	args = append(args, "-G")
	if t.SSHPort != 0 {
		args = append(args, "-p", strconv.Itoa(t.SSHPort))
	}
	args = append(args, "--", t.User+"@"+t.Host)
	runCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "ssh", args...)
	cmd.Stdin = strings.NewReader("")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("targets: read the SSH config: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	cfg := map[string][]string{}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), " ")
		if ok {
			cfg[key] = append(cfg[key], value)
		}
	}
	return cfg, sc.Err()
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// socksProxyAddr is the SOCKS5 proxy a proxy command goes through, or ""
// if it isn't one Loomux knows.
func socksProxyAddr(command string) string {
	for _, re := range socksProxyCommands {
		if m := re.FindStringSubmatch(command); m != nil {
			if host, port, err := net.SplitHostPort(m[1]); err == nil && validHostPort(host, port) {
				return m[1]
			}
		}
	}
	return ""
}

// expandHome expands a leading ~/ the way ssh does.
func expandHome(path, home string) string {
	if home != "" && strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// importIdentity imports the first of files that exists — the key ssh
// would offer first. It must be a regular file directly in ~/.ssh, and
// not passphrase-protected.
func importIdentity(files []string, home, sshDir string) (string, *registry.SSHKey, error) {
	for _, f := range files {
		path := filepath.Clean(expandHome(f, home))
		fi, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		switch {
		case err != nil:
			return path, nil, fmt.Errorf("the SSH config's key file %s can't be read: %v", path, err)
		case sshDir == "" || filepath.Dir(path) != sshDir:
			return path, nil, fmt.Errorf("the SSH config's key file %s isn't in %s; only keys there are imported", path, sshDir)
		case !fi.Mode().IsRegular() || fi.Size() > maxKeyFileSize:
			return path, nil, fmt.Errorf("the SSH config's key file %s isn't a regular key file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return path, nil, fmt.Errorf("the SSH config's key file %s can't be read: %v", path, err)
		}
		key, err := importKey(data, filepath.Base(path))
		if err != nil {
			return path, nil, fmt.Errorf("the SSH config's key file %s: %v", path, err)
		}
		return path, key, nil
	}
	return "", nil, fmt.Errorf("none of the SSH config's key files exist (%s)", strings.Join(files, ", "))
}

// importKey turns a private key file's contents into a managed key, in
// OpenSSH format, named after the file.
func importKey(data []byte, fileName string) (*registry.SSHKey, error) {
	raw, err := ssh.ParseRawPrivateKey(data)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, errors.New("it is passphrase-protected; Loomux can't import it")
	}
	if err != nil {
		return nil, errors.New("it isn't a private key ssh can read")
	}
	if p, ok := raw.(*ed25519.PrivateKey); ok {
		raw = *p
	}
	signer, err := ssh.NewSignerFromKey(raw)
	if err != nil {
		return nil, fmt.Errorf("unsupported key: %v", err)
	}
	const comment = "loomux-imported"
	block, err := ssh.MarshalPrivateKey(raw, comment)
	if err != nil {
		return nil, fmt.Errorf("unsupported key: %v", err)
	}
	pub := signer.PublicKey()
	name := "imported-" + strings.Trim(regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(fileName, "-"), "-._")
	if len(name) > 64 {
		name = name[:64]
	}
	return &registry.SSHKey{
		Name:        name,
		Type:        pub.Type(),
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + comment,
		Fingerprint: ssh.FingerprintSHA256(pub),
		Origin:      registry.SSHKeyOriginImported,
		PrivateKey:  pem.EncodeToMemory(block),
	}, nil
}

// rekeyHostKeys rewrites known_hosts lines (hashed, aliased or not) as
// pins for addr, the address a managed target is checked against.
func rekeyHostKeys(lines, addr string) (string, error) {
	var out []string
	for _, line := range strings.Split(lines, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		marker, _, key, _, _, err := ssh.ParseKnownHosts([]byte(line))
		if err != nil {
			return "", fmt.Errorf("a known_hosts line for this target doesn't parse: %v", err)
		}
		if marker != "" { // @revoked or @cert-authority: not a plain host key
			continue
		}
		out = append(out, knownhosts.Line([]string{addr}, key))
	}
	return strings.Join(out, "\n"), nil
}

// lookupKnownHosts finds the target's host keys in the known_hosts files
// ssh would check, under the name it would look up: the HostKeyAlias if
// one is set, else the host and (if not 22) port.
func lookupKnownHosts(ctx context.Context, cfg map[string][]string, plan *SSHConfigPlan, home, addr string) (string, error) {
	name := first(cfg["hostkeyalias"])
	if name == "" || name == "none" {
		name = knownhosts.Normalize(net.JoinHostPort(plan.Host, strconv.Itoa(plan.Port)))
	}
	var files []string
	for _, k := range []string{"userknownhostsfile", "globalknownhostsfile"} {
		files = append(files, strings.Fields(first(cfg[k]))...)
	}
	var found []string
	for _, f := range files {
		f = expandHome(f, home)
		if _, err := os.Stat(f); err != nil {
			continue
		}
		runCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
		out, err := exec.CommandContext(runCtx, "ssh-keygen", "-F", name, "-f", f).Output()
		cancel()
		if err != nil {
			continue // exit 1: not in this file
		}
		found = append(found, string(out))
	}
	return rekeyHostKeys(strings.Join(found, "\n"), addr)
}
