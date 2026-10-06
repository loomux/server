package targets

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

const pqBanner = `** WARNING: connection is not using a post-quantum key exchange algorithm.
** This session may be vulnerable to "store now, decrypt later" attacks.
** The server may need to be upgraded. See https://openssh.com/pq.html
`

func TestClassifySSHFailure(t *testing.T) {
	cases := []struct {
		name, stderr string
		want         SSHFailure
		wantDetail   string
	}{
		{"host key changed", `@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!
Someone could be eavesdropping on you right now (man-in-the-middle attack)!
It is also possible that a host key has just been changed.
The fingerprint for the ED25519 key sent by the remote host is
SHA256:abc.
Please contact your system administrator.
Add correct host key in /home/loomux/.ssh/known_hosts to get rid of this message.
Offending ED25519 key in /home/loomux/.ssh/known_hosts:3
Host key for jet01 has changed and you have requested strict checking.
Host key verification failed.
`, SSHHostKeyChanged, "Offending ED25519 key in /home/loomux/.ssh/known_hosts:3; Host key for jet01 has changed and you have requested strict checking.; Host key verification failed."},
		{"host key unknown", "No ED25519 host key is known for jet01 and you have requested strict checking.\nHost key verification failed.\n", SSHHostKeyUnknown, ""},
		{"auth", "loomux@jet01: Permission denied (publickey).\n", SSHAuthFailed, "loomux@jet01: Permission denied (publickey)."},
		{"auth behind the pq banner", pqBanner + "orski@sc1: Permission denied (publickey,password).\n", SSHAuthFailed, "orski@sc1: Permission denied (publickey,password)."},
		{"dns", "ssh: Could not resolve hostname nosuch: Name or service not known\n", SSHDNSFailed, ""},
		{"socat proxy down", "2026/10/05 20:54:24 socat[370065] W connect(5, AF=2 127.0.0.1:1055, 16): Connection refused\n2026/10/05 20:54:24 socat[370065] E SOCKS5-CONNECT:127.0.0.1:1055:...: Connection refused\nConnection closed by UNKNOWN port 65535\n", SSHProxyUnreachable, ""},
		{"nc proxy down", "Connection closed by UNKNOWN port 65535\n", SSHProxyUnreachable, ""},
		{"socks refused", "nc: connection failed, SOCKS error 5\nConnection closed by UNKNOWN port 65535\n", SSHConnectionRefused, ""},
		{"socks host unreachable", "nc: connection failed, SOCKS error 4\nConnection closed by UNKNOWN port 65535\n", SSHHostUnreachable, ""},
		{"refused", "ssh: connect to host 127.0.0.1 port 1: Connection refused\n", SSHConnectionRefused, ""},
		{"no route", "ssh: connect to host 10.0.0.9 port 22: No route to host\n", SSHHostUnreachable, ""},
		{"timeout", "ssh: connect to host 10.255.255.1 port 22: Connection timed out\n", SSHTimeout, ""},
		{"banner exchange timeout", "Connection timed out during banner exchange\n", SSHTimeout, ""},
		{"mux refused", "mux_client_request_session: session request failed: Session open refused by peer\n", SSHMuxSessionRefused, ""},
		{"unknown", pqBanner + "something odd\n", SSHOther, "something odd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := classifySSHFailure(tc.stderr)
			if got != tc.want {
				t.Errorf("class = %q, want %q (detail %q)", got, tc.want, detail)
			}
			if strings.Contains(detail, "post-quantum") || strings.Contains(detail, "@@@") {
				t.Errorf("detail keeps a banner line: %q", detail)
			}
			if tc.wantDetail != "" && detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}

func TestClassifySSHFailure_DetailCutOnRuneBoundary(t *testing.T) {
	_, detail := classifySSHFailure(strings.Repeat("é", maxDetailBytes))
	if !utf8.ValidString(detail) {
		t.Errorf("detail isn't valid UTF-8: %q", detail)
	}
}

func TestStripSSHBanner(t *testing.T) {
	if got := stripSSHBanner(pqBanner + "tmux: no server running\n** kept\n"); got != "tmux: no server running\n** kept\n" {
		t.Errorf("stripSSHBanner = %q", got)
	}
	if got := stripSSHBanner(pqBanner); got != "" {
		t.Errorf("stripSSHBanner(banner only) = %q", got)
	}
}

func TestUnreachableError(t *testing.T) {
	err := error(&UnreachableError{Host: "jet01", Failure: SSHHostKeyChanged, Detail: "Host key verification failed."})
	if !errors.Is(err, ErrUnreachable) {
		t.Error("an UnreachableError isn't ErrUnreachable")
	}
	wrapped := errors.Join(errors.New("dispatch"), err)
	u, ok := AsUnreachable(wrapped)
	if !ok || u.Failure != SSHHostKeyChanged {
		t.Fatalf("AsUnreachable = %+v, %v", u, ok)
	}
	msg := err.Error()
	for _, want := range []string{"target unreachable", "host key of jet01 has changed", "known_hosts", "host_key_changed", "Host key verification failed."} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, missing %q", msg, want)
		}
	}
	if got := opErrorReason(wrapped); got != "unreachable_host_key_changed" {
		t.Errorf("opErrorReason = %q", got)
	}
}
