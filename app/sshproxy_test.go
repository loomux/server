package app

import "testing"

// LOOM-138: LOOMUX_SSH_PROXY is the SOCKS5 proxy managed targets are
// reached through: socks5://host:port and nothing else.
func TestLoadConfig_SSHProxy(t *testing.T) {
	setRouterEnv(t)
	for raw, want := range map[string]string{
		"":                        "",
		"socks5://127.0.0.1:1055": "127.0.0.1:1055",
		"socks5://localhost:1080": "localhost:1080",
	} {
		t.Setenv(envSSHProxy, raw)
		cfg, err := LoadConfig()
		if err != nil || cfg.SSHProxy != want {
			t.Errorf("%q: SSHProxy = %q, %v; want %q", raw, cfg.SSHProxy, err, want)
		}
	}
	for _, raw := range []string{"127.0.0.1:1055", "http://127.0.0.1:1055", "socks5://127.0.0.1", "socks5://u:p@127.0.0.1:1055",
		"socks5://127.0.0.1:1055/x", "socks5://127.0.0.1:1055?x=1", "socks5://h;id:1055", "socks5://%68:1055"} {
		t.Setenv(envSSHProxy, raw)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}
