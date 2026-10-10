package docker

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// engineKind is how the plugin reaches the engine.
type engineKind string

const (
	// engineSSH: ssh://user@host[:port], then docker system dial-stdio
	// on the host (what the Docker CLI's ssh:// contexts do).
	engineSSH engineKind = "ssh"
	// engineUnix: the engine's socket on this machine.
	engineUnix engineKind = "unix"
)

// EngineAddr is the parsed "engine" setting.
type EngineAddr struct {
	Kind engineKind
	User string // ssh
	Host string // ssh
	Port int    // ssh
	Path string // unix
}

// String is the address without credentials, for messages and logs.
func (e EngineAddr) String() string {
	if e.Kind == engineUnix {
		return "unix://" + e.Path
	}
	return "ssh://" + e.User + "@" + net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// Config is the plugin's configuration, parsed from what the host sends
// at plugin.configure (the manifest's config_schema, defaults applied).
type Config struct {
	Engine          EngineAddr
	Proxy           string // host:port of a SOCKS5 proxy; "" for none
	BindAddress     string // the docker host's IP machines publish sshd on
	SSHProxy        string // how the host reaches the machines
	AgentImage      string
	Timezone        string
	Sizes           []protocol.Size
	MaxEnvironments int

	// signer is the plugin's own key to the docker host; hostKey the
	// docker host's pinned key, nil until the user trusts the scanned
	// one. Neither is ever written out.
	signer  ssh.Signer
	hostKey ssh.PublicKey
}

var (
	sizeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// hostName is one DNS label; a host is labels joined by dots, or an
	// IP literal. Nothing ssh could read as an option or a shell as
	// syntax.
	hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	userName  = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
)

// defaultSizes is what the plugin offers unless "sizes" says otherwise.
var defaultSizes = []protocol.Size{
	{Name: "small", CPU: "1", Memory: "2Gi", Disk: "10Gi"},
	{Name: "medium", CPU: "2", Memory: "4Gi", Disk: "20Gi"},
	{Name: "large", CPU: "4", Memory: "8Gi", Disk: "40Gi"},
}

func invalid(format string, args ...any) error {
	return &rpc.Error{Code: rpc.CodeInvalidConfig, Message: fmt.Sprintf(format, args...)}
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 || strings.HasPrefix(host, "-") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if !hostLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// parseConfig validates the configuration. Its errors are invalid_config
// and never repeat the private key.
func parseConfig(m map[string]any) (Config, error) {
	c := Config{
		Proxy: str(m, "proxy"), BindAddress: str(m, "bind_address"), SSHProxy: str(m, "ssh_proxy"),
		AgentImage: str(m, "agent_image"), Timezone: str(m, "timezone"), MaxEnvironments: 5,
	}
	engine, err := parseEngine(str(m, "engine"))
	if err != nil {
		return c, err
	}
	c.Engine = engine
	if key := str(m, "ssh_private_key"); engine.Kind == engineSSH {
		if key == "" {
			return c, invalid("ssh_private_key is required for an ssh engine: generate one and add its public half to %s's authorized_keys on the docker host", engine.User)
		}
		signer, err := ssh.ParsePrivateKey([]byte(key))
		if err != nil {
			if _, ok := err.(*ssh.PassphraseMissingError); ok {
				return c, invalid("ssh_private_key must not have a passphrase")
			}
			return c, invalid("ssh_private_key isn't a private key the client accepts (an OpenSSH or PEM private key, no passphrase)")
		}
		c.signer = signer
	}
	if line := str(m, "ssh_host_key"); line != "" {
		pub, err := parseHostKeyLine(line)
		if err != nil {
			return c, invalid("ssh_host_key must be a host key line: ssh-ed25519 AAAA…, or a known_hosts line")
		}
		c.hostKey = pub
	}
	if c.Proxy != "" {
		hostport, err := parseSOCKS5(c.Proxy)
		if err != nil {
			return c, err
		}
		c.Proxy = hostport
	}
	ip := net.ParseIP(c.BindAddress)
	switch {
	case c.BindAddress == "":
		return c, invalid("bind_address is required: the docker host's IP address machines publish sshd on")
	case ip == nil:
		return c, invalid("bind_address must be an IP address of the docker host (Docker publishes ports on an address, not a name)")
	case ip.IsUnspecified():
		return c, invalid("bind_address must be one address of the docker host, not %s: machines would be reachable on every interface", c.BindAddress)
	}
	switch c.SSHProxy {
	case "":
		c.SSHProxy = protocol.SSHProxyDefault
	case protocol.SSHProxyDefault, protocol.SSHProxyNone:
	default:
		return c, invalid("ssh_proxy must be default or none")
	}
	if c.AgentImage == "" || strings.ContainsAny(c.AgentImage, " \t\r\n\"'`$") {
		return c, invalid("agent_image must be an image reference")
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if len(c.Timezone) > 64 || strings.ContainsAny(c.Timezone, " \t\r\n\"'`$;") {
		return c, invalid("timezone must be a tz name such as Europe/Warsaw")
	}
	switch v := m["max_environments"].(type) {
	case nil:
	case float64:
		c.MaxEnvironments = int(v)
	case int:
		c.MaxEnvironments = v
	default:
		return c, invalid("max_environments must be an integer")
	}
	if c.MaxEnvironments < 1 {
		return c, invalid("max_environments must be at least 1")
	}
	sizes, err := parseSizes(m["sizes"])
	if err != nil {
		return c, err
	}
	c.Sizes = sizes
	return c, nil
}

// parseEngine reads ssh://user@host[:port] or unix:///path. Nothing
// else: a TCP endpoint without TLS is an open root shell, and TLS client
// certificates aren't supported (design §4).
func parseEngine(s string) (EngineAddr, error) {
	if s == "" {
		return EngineAddr{}, invalid("engine is required: ssh://user@host[:port] or unix:///var/run/docker.sock")
	}
	if strings.ContainsAny(s, " \t\r\n\"'`$;|&<>\\") {
		return EngineAddr{}, invalid("engine must be ssh://user@host[:port] or unix:///path")
	}
	u, err := url.Parse(s)
	if err != nil {
		return EngineAddr{}, invalid("engine must be ssh://user@host[:port] or unix:///path")
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		if u.Host != "" || !strings.HasPrefix(path, "/") {
			return EngineAddr{}, invalid("a unix engine is unix:///absolute/path/to/docker.sock")
		}
		return EngineAddr{Kind: engineUnix, Path: path}, nil
	case "ssh":
		if u.User == nil || u.User.Username() == "" || !userName.MatchString(u.User.Username()) {
			return EngineAddr{}, invalid("an ssh engine names the user: ssh://user@host[:port]")
		}
		if _, set := u.User.Password(); set {
			return EngineAddr{}, invalid("engine must not carry a password: the plugin authenticates with ssh_private_key")
		}
		if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
			return EngineAddr{}, invalid("engine must be ssh://user@host[:port], nothing after the host")
		}
		host, port := u.Hostname(), 22
		if !validHost(host) {
			return EngineAddr{}, invalid("engine's host must be a host name or IP address")
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return EngineAddr{}, invalid("engine's port must be 1-65535")
			}
			port = n
		}
		return EngineAddr{Kind: engineSSH, User: u.User.Username(), Host: host, Port: port}, nil
	}
	return EngineAddr{}, invalid("engine must be ssh://user@host[:port] or unix:///path; a tcp:// engine isn't supported (no TLS client certificates, design §4)")
}

// parseSOCKS5 reads socks5://host:port into host:port.
func parseSOCKS5(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "socks5" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Port() == "" || !validHost(u.Hostname()) {
		return "", invalid("proxy must be socks5://host:port")
	}
	if n, err := strconv.Atoi(u.Port()); err != nil || n < 1 || n > 65535 {
		return "", invalid("proxy must be socks5://host:port")
	}
	return net.JoinHostPort(u.Hostname(), u.Port()), nil
}

// parseHostKeyLine reads a host key as "type base64 [comment]" or as a
// known_hosts line "host type base64 [comment]".
func parseHostKeyLine(line string) (ssh.PublicKey, error) {
	if pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil {
		return pub, nil
	}
	_, _, pub, _, _, err := ssh.ParseKnownHosts([]byte(line))
	if err != nil {
		return nil, err
	}
	return pub, nil
}

func parseSizes(v any) ([]protocol.Size, error) {
	m, ok := v.(map[string]any)
	if v != nil && !ok {
		return nil, invalid("sizes must be an object of {cpu, memory, disk} by name")
	}
	if len(m) == 0 {
		return append([]protocol.Size(nil), defaultSizes...), nil
	}
	out := make([]protocol.Size, 0, len(m))
	for name, raw := range m {
		if !sizeName.MatchString(name) {
			return nil, invalid("size %q: the name must be lower-case letters, digits and dashes", name)
		}
		fields, ok := raw.(map[string]any)
		if !ok {
			return nil, invalid("size %q must be an object of {cpu, memory, disk}", name)
		}
		s := protocol.Size{Name: name, CPU: str(fields, "cpu"), Memory: str(fields, "memory"), Disk: str(fields, "disk")}
		if s.CPU == "" || s.Memory == "" || s.Disk == "" {
			return nil, invalid("size %q: cpu, memory and disk are required", name)
		}
		if _, err := parseCPU(s.CPU); err != nil {
			return nil, invalid("size %q: cpu %q isn't a quantity (1, 2, 500m)", name, s.CPU)
		}
		for _, q := range []struct{ k, v string }{{"memory", s.Memory}, {"disk", s.Disk}} {
			if _, err := parseBytes(q.v); err != nil {
				return nil, invalid("size %q: %s %q isn't a quantity (2Gi, 512Mi, 1G)", name, q.k, q.v)
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		ci, _ := parseCPU(out[i].CPU)
		cj, _ := parseCPU(out[j].CPU)
		if ci != cj {
			return ci < cj
		}
		mi, _ := parseBytes(out[i].Memory)
		mj, _ := parseBytes(out[j].Memory)
		if mi != mj {
			return mi < mj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// size is the named size, if offered.
func (c Config) size(name string) (protocol.Size, bool) {
	for _, s := range c.Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return protocol.Size{}, false
}

func (c Config) sizeNames() string {
	names := make([]string, 0, len(c.Sizes))
	for _, s := range c.Sizes {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// addressTemplate is where machines are reachable: the docker host's
// bind address, the same for every machine; the port is each machine's
// own, known once it is made (design §2.3).
func (c Config) addressTemplate() string { return c.BindAddress }

func (c Config) address(port int) protocol.Address {
	return protocol.Address{Host: c.BindAddress, Port: port, Proxy: c.SSHProxy}
}
