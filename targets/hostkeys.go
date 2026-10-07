package targets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/registry"
)

// Host keys pinned through the API (LOOM-114). A pin is additive to the
// mounted SSH secret: a pinned target is checked against its pin alone,
// written to a known_hosts file of its own; every other target against
// the mounted known_hosts as before. The database holds the pin; the
// file is written from it whenever the target's executor is built.

// knownHostsDir is where pinned targets' known_hosts files are written.
var (
	knownHostsMu  sync.Mutex
	knownHostsDir = filepath.Join(os.TempDir(), "loomux", "known_hosts.d")
)

// SetKnownHostsDir sets where pinned host keys are written (the data
// volume, beside the database). Call it once at startup.
func SetKnownHostsDir(dir string) {
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()
	knownHostsDir = dir
}

// HostKey is one host key: its known_hosts line, its type
// ("ssh-ed25519") and its SHA256 fingerprint as ssh-keygen -l prints it.
type HostKey struct {
	Line        string
	Type        string
	Fingerprint string
}

// ParseHostKeys reads known_hosts lines (as pinned, or as a scan wrote
// them); comments and blank lines are skipped.
func ParseHostKeys(lines string) ([]HostKey, error) {
	var out []HostKey
	for _, line := range strings.Split(lines, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, _, key, _, _, err := ssh.ParseKnownHosts([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("targets: host key line: %w", err)
		}
		out = append(out, HostKey{Line: line, Type: key.Type(), Fingerprint: ssh.FingerprintSHA256(key)})
	}
	return out, nil
}

// pinnedKnownHosts writes t's pinned keys to its known_hosts file, if
// they aren't there already, and returns the file's path.
func pinnedKnownHosts(t *registry.Target) (string, error) {
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()
	if err := os.MkdirAll(knownHostsDir, 0o700); err != nil {
		return "", fmt.Errorf("targets: known_hosts dir: %w", err)
	}
	path := filepath.Join(knownHostsDir, sanitizeForFilename(t.ID))
	want := []byte(strings.TrimSpace(t.HostKeys) + "\n")
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return path, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o600); err != nil {
		return "", fmt.Errorf("targets: write known_hosts: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("targets: write known_hosts: %w", err)
	}
	return path, nil
}

// remoteOptions are t's per-target SSH overrides (LOOM-114): its port,
// and its pinned host keys checked strictly and alone.
func remoteOptions(t *registry.Target) ([]RemoteOption, error) {
	var opts []RemoteOption
	if t.SSHPort != 0 {
		opts = append(opts, WithPort(t.SSHPort))
	}
	if strings.TrimSpace(t.HostKeys) != "" {
		path, err := pinnedKnownHosts(t)
		if err != nil {
			return nil, err
		}
		opts = append(opts, WithExtraSSHArgs("-o", "UserKnownHostsFile="+path,
			"-o", "GlobalKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=yes"))
	}
	return opts, nil
}

// ScanHostKey connects to t's host the way Loomux does (the mounted SSH
// config, its proxy, t's port) and returns the host key it presents,
// without authenticating and without trusting it: nothing is pinned
// until a pin names one of the fingerprints (LOOM-114, trust on first
// use, confirmed by a person). ssh records the key once key exchange has
// checked the host holds it, before authentication, which is then
// refused on purpose.
//
// Loomux has one user today. If it ever has several, scanning and
// pinning must become admin-only: whoever pins decides which machine
// every later command reaches.
func ScanHostKey(ctx context.Context, t *registry.Target) ([]HostKey, error) {
	if t.Kind != registry.TargetKindRemote {
		return nil, errors.New("targets: only a remote target has a host key")
	}
	dir, err := os.MkdirTemp("", "loomux-scan-")
	if err != nil {
		return nil, fmt.Errorf("targets: scan: %w", err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "known_hosts")
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=" + defaultConnectTimeout,
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + file,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HashKnownHosts=no",
		"-o", "PreferredAuthentications=none",
	}
	if t.SSHPort != 0 {
		args = append(args, "-p", strconv.Itoa(t.SSHPort))
	}
	dest := t.Host
	if t.User != "" {
		dest = t.User + "@" + t.Host
	}
	args = append(args, "--", dest, "true")
	scanCtx, cancel := context.WithTimeout(ctx, defaultOpTimeout)
	defer cancel()
	cmd := exec.CommandContext(scanCtx, "ssh", args...)
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run() // refused authentication is the expected end

	data, _ := os.ReadFile(file)
	keys, err := ParseHostKeys(string(data))
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		failure, detail := classifySSHFailure(stripSSHBanner(stderr.String()))
		if detail == "" {
			detail = "no host key was received"
		}
		return nil, &UnreachableError{Host: t.Host, Failure: failure, Detail: detail}
	}
	return keys, nil
}
