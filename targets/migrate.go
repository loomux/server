package targets

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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

// keyFileName is what a caller-chosen key file may be: a file name in
// ~/.ssh, nothing else.
var keyFileName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// ResolveSSHConfig reads what the SSH config does for t, a config-mode
// remote target, and plans it as a managed one. keyFile, if not "", is
// the file in ~/.ssh to import instead of the config's first usable one.
func ResolveSSHConfig(ctx context.Context, t *registry.Target, keyFile string) (*SSHConfigPlan, error) {
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
	identities := cfg["identityfile"]
	if keyFile != "" {
		if !keyFileName.MatchString(keyFile) || ssh == "" {
			identities = nil
			problem("key_file must be the name of a file in ~/.ssh, like id_ed25519")
		} else {
			identities = []string{filepath.Join(ssh, keyFile)}
		}
	}
	if identities != nil || keyFile == "" {
		plan.KeyFile, plan.Key, err = importIdentity(identities, home, ssh)
		if err != nil {
			problem("%v", err)
		}
	}

	knownHostsFiles := knownHostsFilesOf(cfg, home)
	revoked := revokedHostKeys(knownHostsFiles)
	addr := knownhosts.Normalize(net.JoinHostPort(plan.Host, strconv.Itoa(plan.Port)))
	if strings.TrimSpace(t.HostKeys) != "" {
		plan.HostKeys, err = rekeyHostKeys(t.HostKeys, addr, revoked)
	} else {
		plan.HostKeys, err = lookupKnownHosts(ctx, cfg, plan, knownHostsFiles, addr, revoked)
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
		// Not ssh's stderr: it can quote the config's lines.
		return nil, fmt.Errorf("targets: read the SSH config: ssh -G failed (%v)", err)
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

// importIdentity imports the first of files that can be — the key ssh
// would offer first, ssh too moving past one it can't use. Only a
// regular, passphrase-free key file directly in ~/.ssh is read.
func importIdentity(files []string, home, sshDir string) (string, *registry.SSHKey, error) {
	var skipped []string
	for _, f := range files {
		path := filepath.Clean(expandHome(f, home))
		if sshDir == "" || filepath.Dir(path) != sshDir {
			skipped = append(skipped, path+": not in "+sshDir+"; only keys there are imported")
			continue
		}
		data, err := readKeyFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			skipped = append(skipped, path+": "+err.Error())
			continue
		}
		key, err := importKey(data, filepath.Base(path))
		if err != nil {
			skipped = append(skipped, path+": "+err.Error())
			continue
		}
		return path, key, nil
	}
	if len(skipped) > 0 {
		return "", nil, fmt.Errorf("no usable key file: %s", strings.Join(skipped, "; "))
	}
	return "", nil, fmt.Errorf("none of the key files exist (%s)", strings.Join(files, ", "))
}

// readKeyFile reads a key file without following a symlink, and only if
// it is a regular file of at most maxKeyFileSize: checked on the open
// file, so it can't be swapped between the check and the read.
func readKeyFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, errors.New("it can't be opened as a regular file (a symlink?)")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("it isn't a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return nil, errors.New("it can't be read")
	}
	if len(data) > maxKeyFileSize {
		return nil, errors.New("it is too big to be a key")
	}
	return data, nil
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
// pins for addr, the address a managed target is checked against,
// leaving out any key in revoked.
func rekeyHostKeys(lines, addr string, revoked map[string]bool) (string, error) {
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
		if marker != "" || revoked[string(key.Marshal())] { // not a plain, trusted host key
			continue
		}
		out = append(out, knownhosts.Line([]string{addr}, key))
	}
	return strings.Join(out, "\n"), nil
}

// knownHostsFilesOf are the known_hosts files ssh would check.
func knownHostsFilesOf(cfg map[string][]string, home string) []string {
	var files []string
	for _, k := range []string{"userknownhostsfile", "globalknownhostsfile"} {
		for _, f := range strings.Fields(first(cfg[k])) {
			files = append(files, expandHome(f, home))
		}
	}
	return files
}

// revokedHostKeys are the keys any of files marks @revoked, whatever
// host pattern it gives: such a key is never pinned.
func revokedHostKeys(files []string) map[string]bool {
	revoked := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "@revoked") {
				continue
			}
			if marker, _, key, _, _, err := ssh.ParseKnownHosts([]byte(line)); err == nil && marker == "revoked" {
				revoked[string(key.Marshal())] = true
			}
		}
	}
	return revoked
}

// lookupKnownHosts finds the target's host keys in the known_hosts files
// ssh would check, under the name it would look up: the HostKeyAlias if
// one is set, else the host and (if not 22) port.
func lookupKnownHosts(ctx context.Context, cfg map[string][]string, plan *SSHConfigPlan, files []string, addr string, revoked map[string]bool) (string, error) {
	name := first(cfg["hostkeyalias"])
	if name == "" || name == "none" {
		name = knownhosts.Normalize(net.JoinHostPort(plan.Host, strconv.Itoa(plan.Port)))
	}
	var found []string
	for _, f := range files {
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
	return rekeyHostKeys(strings.Join(found, "\n"), addr, revoked)
}
