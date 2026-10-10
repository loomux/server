package sshtest_test

import (
	"bytes"
	"io"
	"net"
	"os"
	"strconv"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/targets/sshtest"
)

// An Exec hook serves a command itself (a plugin test's stand-in for
// a program the test host lacks); every other command still reaches the
// login shell.
func TestExecHookServesCommandsItAccepts(t *testing.T) {
	s := sshtest.Start(t)
	s.Exec = func(command string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
		if command != "hooked-command" {
			return 0, false
		}
		in, _ := io.ReadAll(stdin)
		io.WriteString(stdout, "hook saw "+string(in))
		return 7, true
	}
	key, err := os.ReadFile(s.IdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s.HostKey))
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), &ssh.ClientConfig{
		User: "test", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(hostKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	sess.Stdin = bytes.NewBufferString("input")
	var out bytes.Buffer
	sess.Stdout = &out
	err = sess.Run("hooked-command")
	var exit *ssh.ExitError
	if !errorsAs(err, &exit) || exit.ExitStatus() != 7 {
		t.Errorf("hooked command: want exit 7, got %v", err)
	}
	if out.String() != "hook saw input" {
		t.Errorf("stdout = %q", out.String())
	}
	sess.Close()

	sess, err = client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	sess.Stdout = &out
	if err := sess.Run("echo shell"); err != nil {
		t.Fatalf("an unhooked command should reach the shell: %v", err)
	}
	if out.String() != "shell\n" {
		t.Errorf("stdout = %q", out.String())
	}
}

func errorsAs(err error, target **ssh.ExitError) bool {
	e, ok := err.(*ssh.ExitError)
	if ok {
		*target = e
	}
	return ok
}
