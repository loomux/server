package targets

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets/sshtest"
)

// LOOM-114: a scan reports the key the host presents, without trusting
// or authenticating; a pinned key lets the executor in, a different one
// is a host key change.
func TestScanAndPinHostKey(t *testing.T) {
	SetKnownHostsDir(t.TempDir())
	server := sshtest.Start(t)
	ctx := context.Background()
	target := &registry.Target{ID: "t-pin", Kind: registry.TargetKindRemote, Host: server.Host, User: "loomux-test",
		SSHPort: server.Port}

	keys, err := ScanHostKey(ctx, target)
	if err != nil {
		t.Fatalf("ScanHostKey: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(server.HostKey))
	if err != nil {
		t.Fatalf("server host key: %v", err)
	}
	if len(keys) != 1 || keys[0].Fingerprint != ssh.FingerprintSHA256(pub) || keys[0].Type != "ssh-ed25519" {
		t.Fatalf("scanned %+v, want the server's key %s", keys, ssh.FingerprintSHA256(pub))
	}

	run := func(hostKeys string) error {
		target.HostKeys = hostKeys
		opts, err := remoteOptions(target)
		if err != nil {
			t.Fatalf("remoteOptions: %v", err)
		}
		e := NewRemoteExecutor(target.Host, target.User, append(opts, WithIdentityFile(server.IdentityFile))...)
		defer e.Close() // each check makes its own connection
		_, err = e.RunOnce(ctx, "true")
		return err
	}

	other, _, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _ := ssh.NewPublicKey(other)
	wrong := fmt.Sprintf("[%s]:%d %s", server.Host, server.Port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(otherPub))))
	var unreachable *UnreachableError
	if err := run(wrong); !errors.As(err, &unreachable) || unreachable.Failure != SSHHostKeyChanged {
		t.Fatalf("with another key pinned: %v, want host_key_changed", err)
	}
	if !strings.Contains(unreachable.Hint(), "scan and pin") {
		t.Errorf("hint = %q, want it to say how to re-pin", unreachable.Hint())
	}
	if err := run(keys[0].Line); err != nil {
		t.Fatalf("with the scanned key pinned: %v", err)
	}
}

func TestParseHostKeys(t *testing.T) {
	keys, err := ParseHostKeys("# comment\n\nbox ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n")
	if err != nil || len(keys) != 1 || keys[0].Type != "ssh-ed25519" || !strings.HasPrefix(keys[0].Fingerprint, "SHA256:") {
		t.Fatalf("ParseHostKeys = %+v, %v", keys, err)
	}
	if _, err := ParseHostKeys("not a key line"); err == nil {
		t.Fatal("a malformed line was accepted")
	}
}
