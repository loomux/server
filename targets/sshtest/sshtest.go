// Package sshtest is test-only infrastructure: a minimal in-process SSH
// server for exercising the real ssh client binary against a loopback
// target, satisfying the design spec's requirement that TargetExecutor be
// testable against a local/loopback SSH target. It accepts exactly one
// generated client key and, on any "exec" request, actually runs the
// received command locally via /bin/sh -c — so commands a real ssh client
// sends (including tmux invocations) really execute.
package sshtest

import (
	"bytes"
	"crypto/ed25519"
	crand "crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Server is a running loopback SSH test server.
type Server struct {
	Host string // "127.0.0.1"
	Port int

	// IdentityFile is the path to a PEM-encoded private key authorized to
	// log in — pass to `ssh -i`.
	IdentityFile string

	listener net.Listener
	// loginShell runs each exec request's command as `<loginShell> -c
	// <command>`, the way sshd hands it to the user's login shell.
	loginShell string
}

// Start generates a fresh host keypair and a fresh client keypair, starts
// listening on 127.0.0.1 (an ephemeral port), and serves connections
// until the listener is closed via t.Cleanup.
func Start(t *testing.T) *Server {
	t.Helper()
	return StartWithLoginShell(t, "/bin/sh")
}

// StartWithLoginShell is Start with a different login shell for the
// remote "user" — e.g. fish, to prove nothing Loomux sends is parsed by a
// non-POSIX login shell (LOOM-90 review).
func StartWithLoginShell(t *testing.T, loginShell string) *Server {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("sshtest: generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("sshtest: host signer: %v", err)
	}

	clientPub, clientPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("sshtest: generate client key: %v", err)
	}
	authorizedKey, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatalf("sshtest: client public key: %v", err)
	}

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorizedKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("sshtest: unauthorized key")
		},
	}
	config.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sshtest: listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	s := &Server{
		Host:         "127.0.0.1",
		Port:         ln.Addr().(*net.TCPAddr).Port,
		IdentityFile: writeIdentityFile(t, clientPriv),
		listener:     ln,
		loginShell:   loginShell,
	}

	go s.serve(config)

	return s
}

func (s *Server) serve(config *ssh.ServerConfig) {
	for {
		nConn, err := s.listener.Accept()
		if err != nil {
			return // listener closed: normal shutdown
		}
		go handleConn(nConn, config, s.loginShell)
	}
}

func handleConn(nConn net.Conn, config *ssh.ServerConfig, loginShell string) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go handleSession(channel, requests, loginShell)
	}
}

type execRequestPayload struct {
	Command string
}

type exitStatusPayload struct {
	Status uint32
}

// handleSession services exactly one "exec" request per channel by
// running the received command locally, then reports its exit status —
// this is what makes commands sent by a real ssh client (tmux
// invocations included) actually take effect.
func handleSession(channel ssh.Channel, requests <-chan *ssh.Request, loginShell string) {
	defer channel.Close()
	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}

		var payload execRequestPayload
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)

		cmd := exec.Command(loginShell, "-c", payload.Command)
		cmd.Stdout = channel
		cmd.Stderr = channel.Stderr()
		cmd.Stdin = channel

		exitCode := 0
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		channel.SendRequest("exit-status", false, ssh.Marshal(exitStatusPayload{Status: uint32(exitCode)}))
		return
	}
}

func writeIdentityFile(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("sshtest: marshal private key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("sshtest: write identity file: %v", err)
	}
	return path
}
