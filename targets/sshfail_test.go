package targets_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/sshtest"
)

// freshKey is a new ed25519 key: its public half in authorized_keys
// form, and its private half written as an identity file.
func freshKey(t *testing.T) (authorized string, identityFile string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	identityFile = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(identityFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), identityFile
}

// unreachableFailure runs one operation against server with the given
// identity and known_hosts content (strict host key checking) and
// returns the SSH failure class it surfaced as.
func unreachableFailure(t *testing.T, server *sshtest.Server, identityFile, knownHosts string) (*targets.UnreachableError, error) {
	t.Helper()
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(knownHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := targets.NewRemoteExecutor(server.Host, "loomux-test",
		targets.WithPort(server.Port),
		targets.WithIdentityFile(identityFile),
		targets.WithExtraSSHArgs("-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+kh),
	)
	t.Cleanup(func() { exec.Close() })
	_, err := exec.HasSession(context.Background(), "irrelevant")
	u, ok := targets.AsUnreachable(err)
	if !ok {
		return nil, fmt.Errorf("err = %v, want an UnreachableError", err)
	}
	return u, nil
}

// TestRemoteExecutor_SSHFailureClasses (LOOM-85): the failures ssh
// reports with the same exit status 255 come back as distinct classes,
// each with a hint, against a real ssh client and server.
func TestRemoteExecutor_SSHFailureClasses(t *testing.T) {
	server := sshtest.Start(t)
	hostPattern := fmt.Sprintf("[%s]:%d", server.Host, server.Port)
	otherHostKey, _ := freshKey(t)

	t.Run("host key changed", func(t *testing.T) {
		u, err := unreachableFailure(t, server, server.IdentityFile, hostPattern+" "+otherHostKey+"\n")
		if err != nil {
			t.Fatal(err)
		}
		if u.Failure != targets.SSHHostKeyChanged {
			t.Fatalf("failure = %q, want host_key_changed: %v", u.Failure, u)
		}
		if !strings.Contains(u.Error(), "host key of "+server.Host+" has changed") {
			t.Errorf("no hint in %q", u.Error())
		}
	})
	t.Run("host key unknown", func(t *testing.T) {
		u, err := unreachableFailure(t, server, server.IdentityFile, "")
		if err != nil {
			t.Fatal(err)
		}
		if u.Failure != targets.SSHHostKeyUnknown {
			t.Fatalf("failure = %q, want host_key_unknown: %v", u.Failure, u)
		}
	})
	t.Run("key refused", func(t *testing.T) {
		_, stranger := freshKey(t)
		// The real host key, so only the client key is wrong.
		u, err := unreachableFailure(t, server, stranger, hostPattern+" "+server.HostKey+"\n")
		if err != nil {
			t.Fatal(err)
		}
		if u.Failure != targets.SSHAuthFailed {
			t.Fatalf("failure = %q, want auth_failed: %v", u.Failure, u)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		exec := targets.NewRemoteExecutor("127.0.0.1", "loomux-test",
			targets.WithPort(unusedPort(t)),
			targets.WithConnectTimeout("2s"),
			targets.WithExtraSSHArgs("-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null"),
		)
		t.Cleanup(func() { exec.Close() })
		_, err := exec.HasSession(context.Background(), "irrelevant")
		if u, ok := targets.AsUnreachable(err); !ok || u.Failure != targets.SSHConnectionRefused {
			t.Fatalf("err = %v, want connection_refused", err)
		}
	})
}
