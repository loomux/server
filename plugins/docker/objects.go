package docker

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// Labels (design §9: loomux.io/* on Docker objects). Every object the
// plugin makes carries them, so it only ever touches its own.
const (
	managedBy      = "loomux-plugin-docker"
	labelManagedBy = "loomux.io/managed-by"
	labelRole      = "loomux.io/role"
	labelPort      = "loomux.io/ssh-port"
	labelName      = "loomux.io/target-name"
	labelSpec      = "loomux.io/spec"
	labelCreated   = "loomux.io/created-at"
	roleAgent      = "agent"
	roleInit       = "init"

	// networkName is the one user-defined bridge every machine joins,
	// with container-to-container traffic off.
	networkName = "loomux-agents"
	agentUser   = "10002:10002"
	agentUID    = 10002
	sshPort     = 2222
	// sshSrcDir is where the machine finds sshd's two files (the
	// record volume, read-only); helperMount where the helper writes
	// them (the same volume, read-write).
	sshSrcDir   = "/etc/loomux/ssh-src"
	helperMount = "/ssh"
	pidsLimit   = int64(512)
)

var (
	// idPattern: lx-<id> must be a name Docker and DNS accept, so an id
	// starts and ends alphanumeric.
	idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
)

func containerName(id string) string { return "lx-" + id }
func helperName(id string) string    { return "lx-" + id + "-init" }
func recordName(id string) string    { return "lx-" + id + "-ssh" }
func dataName(id string) string      { return "lx-" + id + "-data" }

// Engine API request and response shapes: only the fields the plugin
// sends or reads, named as the API names them.

type containerCreate struct {
	Hostname     string              `json:"Hostname,omitempty"`
	User         string              `json:"User"`
	Env          []string            `json:"Env"`
	Image        string              `json:"Image"`
	Labels       map[string]string   `json:"Labels"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Healthcheck  *healthConfig       `json:"Healthcheck,omitempty"`
	HostConfig   hostConfig          `json:"HostConfig"`
}

type healthConfig struct {
	Test        []string      `json:"Test"`
	Interval    time.Duration `json:"Interval"`
	Timeout     time.Duration `json:"Timeout"`
	StartPeriod time.Duration `json:"StartPeriod"`
	Retries     int           `json:"Retries"`
}

type hostConfig struct {
	NetworkMode    string                   `json:"NetworkMode"`
	PortBindings   map[string][]portBinding `json:"PortBindings,omitempty"`
	RestartPolicy  restartPolicy            `json:"RestartPolicy"`
	Mounts         []mount                  `json:"Mounts"`
	Tmpfs          map[string]string        `json:"Tmpfs,omitempty"`
	ReadonlyRootfs bool                     `json:"ReadonlyRootfs"`
	CapDrop        []string                 `json:"CapDrop"`
	SecurityOpt    []string                 `json:"SecurityOpt"`
	PidsLimit      *int64                   `json:"PidsLimit,omitempty"`
	Memory         int64                    `json:"Memory,omitempty"`
	MemorySwap     int64                    `json:"MemorySwap,omitempty"`
	NanoCPUs       int64                    `json:"NanoCpus,omitempty"`
	Init           *bool                    `json:"Init,omitempty"`
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type restartPolicy struct {
	Name string `json:"Name"`
}

type mount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source,omitempty"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

type containerInspect struct {
	ID           string    `json:"Id"`
	Name         string    `json:"Name"`
	Created      time.Time `json:"Created"`
	Image        string    `json:"Image"`
	RestartCount int       `json:"RestartCount"`
	State        struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		ExitCode  int    `json:"ExitCode"`
		Error     string `json:"Error"`
		StartedAt string `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings map[string][]portBinding `json:"PortBindings"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Ports map[string][]portBinding `json:"Ports"`
	} `json:"NetworkSettings"`
}

type containerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

type volumeInfo struct {
	Name      string            `json:"Name"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt string            `json:"CreatedAt"`
}

type volumesList struct {
	Volumes []volumeInfo `json:"Volumes"`
}

type networkInfo struct {
	ID      string            `json:"Id"`
	Name    string            `json:"Name"`
	Options map[string]string `json:"Options"`
	Labels  map[string]string `json:"Labels"`
}

type imageInspect struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
}

type versionInfo struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	OS            string `json:"Os"`
}

type infoResult struct {
	ServerVersion   string   `json:"ServerVersion"`
	OSType          string   `json:"OSType"`
	SecurityOptions []string `json:"SecurityOptions"`
	InitBinary      string   `json:"InitBinary"`
}

// labelsFor are the identity labels of every object of one machine.
func labelsFor(id, targetID, instance string) map[string]string {
	return map[string]string{
		labelManagedBy:            managedBy,
		protocol.LabelInstance:    instance,
		protocol.LabelTarget:      targetID,
		protocol.LabelEnvironment: id,
	}
}

// labelFilter is the engine filter for this instance's objects with
// role (all roles when "").
func labelFilter(instance, role string) url.Values {
	labels := []string{labelManagedBy + "=" + managedBy, protocol.LabelInstance + "=" + instance}
	if role != "" {
		labels = append(labels, labelRole+"="+role)
	}
	js, _ := json.Marshal(map[string][]string{"label": labels})
	return url.Values{"filters": {string(js)}}
}

// cleanText is user text fit for a label: one line, bounded.
func cleanText(s string) string {
	return truncate(strings.Join(strings.Fields(s), " "), 200)
}

// labelValueOK: a label value is one line of printable text, bounded.
func labelValueOK(s string) bool {
	if len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// validateSpec refuses what can't become object names or labels, an
// egress the plugin can't honour, and a spec that claims another
// instance: the host's id from configure is the authority.
func validateSpec(spec protocol.EnvironmentSpec, cfg Config, host protocol.HostInfo) error {
	if !idPattern.MatchString(spec.ID) {
		return invalidParams("id must be lower-case letters, digits and dashes, at most 40, not ending in a dash")
	}
	if !labelValueOK(spec.TargetID) {
		return invalidParams("target_id isn't a label value")
	}
	if _, ok := cfg.size(spec.Size); !ok {
		return invalidParams("size %q isn't offered; one of %s", spec.Size, cfg.sizeNames())
	}
	switch spec.Egress {
	case protocol.EgressInternet:
	case protocol.EgressNone:
		return invalidParams("this plugin can't confine a machine's egress: Docker publishes no port on an internal network, so sshd would be unreachable; use internet")
	default:
		return invalidParams("egress must be internet")
	}
	if spec.Image != "" && strings.ContainsAny(spec.Image, " \t\r\n\"'`$") {
		return invalidParams("image must be an image reference")
	}
	if strings.TrimSpace(spec.SSH.AuthorizedKey) == "" || strings.TrimSpace(spec.SSH.HostPrivateKey) == "" {
		return invalidParams("ssh.authorized_key and ssh.host_private_key are required")
	}
	for k, v := range spec.Labels {
		if !labelValueOK(v) || !labelValueOK(k) {
			return invalidParams("label %s isn't a label value", cleanText(k))
		}
	}
	if v := spec.Labels[protocol.LabelInstance]; v != "" && v != host.InstanceID {
		return invalidParams("the spec is labelled for another Loomux instance")
	}
	if host.InstanceID == "" || !labelValueOK(host.InstanceID) {
		return invalidParams("the host's instance id isn't a label value")
	}
	return nil
}

func invalidParams(format string, args ...any) error {
	return &rpc.Error{Code: rpc.CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// storedSpec is what the record remembers of the spec: everything but
// the key material, which is the record volume's contents.
func storedSpec(spec protocol.EnvironmentSpec) protocol.EnvironmentSpec {
	spec.SSH = protocol.SSHBootstrap{Port: spec.SSH.Port}
	return spec
}

// recordLabels are the record volume's labels: the machine's identity,
// its spec (without key material), its fixed ssh port and when it was
// made, so start and recreate need only the id.
func recordLabels(spec protocol.EnvironmentSpec, instance string, port int, now time.Time) map[string]string {
	js, _ := json.Marshal(storedSpec(spec))
	labels := labelsFor(spec.ID, spec.TargetID, instance)
	labels[labelRole] = roleAgent
	labels[labelName] = cleanText(spec.Name)
	labels[labelSpec] = string(js)
	labels[labelPort] = strconv.Itoa(port)
	labels[labelCreated] = now.UTC().Format(time.RFC3339)
	return labels
}

// specFrom is the spec a record remembers.
func specFrom(labels map[string]string) (protocol.EnvironmentSpec, error) {
	var spec protocol.EnvironmentSpec
	if err := json.Unmarshal([]byte(labels[labelSpec]), &spec); err != nil || spec.ID == "" {
		return spec, &rpc.Error{Code: rpc.CodeInternal, Message: "the machine's record on the docker host is damaged (volume labels)"}
	}
	return spec, nil
}

// portFrom is the record's fixed ssh port, 0 when unknown.
func portFrom(labels map[string]string) int {
	n, err := strconv.Atoi(labels[labelPort])
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

func createdFrom(labels map[string]string, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, labels[labelCreated]); err == nil {
		return t
	}
	return fallback
}

// hardened is what every container the plugin makes runs under
// (design §4, §9): no capabilities, no privilege escalation, a
// read-only root, the agent user. The daemon's default seccomp profile
// applies (the API takes no "default" value; check warns when the
// daemon runs without seccomp).
func hardened(h *hostConfig) {
	h.ReadonlyRootfs = true
	h.CapDrop = []string{"ALL"}
	h.SecurityOpt = []string{"no-new-privileges:true"}
}

// containerFor is the machine's container, field by field as design §9
// has it, with what Docker made necessary: the record volume mounted
// read-only where the entrypoint looks (an archive can't be uploaded
// into a read-only rootfs), the fixed host port (an ephemeral one is
// reallocated at every start), docker-init as process 1 (sshd reaps
// nothing, and zombies count against the pids limit), and a health
// check standing in for the readiness probe.
func containerFor(spec protocol.EnvironmentSpec, size protocol.Size, cfg Config, instance string, port int) containerCreate {
	image := spec.Image
	if image == "" {
		image = cfg.AgentImage
	}
	memory, _ := parseBytes(size.Memory)
	cpus, _ := parseCPU(size.CPU)
	labels := labelsFor(spec.ID, spec.TargetID, instance)
	labels[labelRole] = roleAgent
	labels[labelName] = cleanText(spec.Name)
	labels[labelPort] = strconv.Itoa(port)
	data := mount{Type: "volume", Target: "/data"}
	if spec.Persistent {
		data.Source = dataName(spec.ID)
	}
	h := hostConfig{
		NetworkMode:   networkName,
		PortBindings:  map[string][]portBinding{"2222/tcp": {{HostIP: cfg.BindAddress, HostPort: strconv.Itoa(port)}}},
		RestartPolicy: restartPolicy{Name: "unless-stopped"},
		Mounts: []mount{
			{Type: "volume", Source: recordName(spec.ID), Target: sshSrcDir, ReadOnly: true},
			data,
		},
		Tmpfs:      map[string]string{"/tmp": "size=1g", "/run/loomux": "size=1m"},
		PidsLimit:  ptr(pidsLimit),
		Memory:     memory,
		MemorySwap: memory,
		NanoCPUs:   cpus,
		Init:       ptr(true),
	}
	hardened(&h)
	return containerCreate{
		Hostname:     containerName(spec.ID),
		User:         agentUser,
		Env:          []string{"HOME=/data/home", "CLAUDE_CONFIG_DIR=/data/home/.claude", "TZ=" + cfg.Timezone},
		Image:        image,
		Labels:       labels,
		ExposedPorts: map[string]struct{}{"2222/tcp": {}},
		Healthcheck: &healthConfig{
			Test: []string{"CMD", "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/2222"}, Interval: 5 * time.Second, Timeout: 3 * time.Second, StartPeriod: 5 * time.Second, Retries: 3,
		},
		HostConfig: h,
	}
}

// helperMode is what a helper is for.
type helperMode int

const (
	// helperProbe publishes sshd's port on an ephemeral host port and
	// sleeps, so the plugin can read which port Docker gives and fix
	// it. It mounts nothing: a mount of the record volume would make
	// the daemon create it first, without the record's labels.
	helperProbe helperMode = iota
	// helperUpload mounts the record volume read-write to receive
	// sshd's files; it is never started.
	helperUpload
)

// helperFor is the transient helper of one machine, hardened like the
// machine, removed in the same call that made it.
func helperFor(id, image string, cfg Config, instance string, mode helperMode) containerCreate {
	if image == "" {
		image = cfg.AgentImage
	}
	labels := labelsFor(id, "", instance)
	labels[labelRole] = roleInit
	delete(labels, protocol.LabelTarget)
	h := hostConfig{
		NetworkMode:   networkName,
		RestartPolicy: restartPolicy{Name: "no"},
		Mounts:        []mount{},
		PidsLimit:     ptr(int64(16)),
	}
	c := containerCreate{
		User: agentUser, Env: []string{}, Image: image, Labels: labels,
		Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"},
	}
	switch mode {
	case helperProbe:
		h.PortBindings = map[string][]portBinding{"2222/tcp": {{HostIP: cfg.BindAddress, HostPort: "0"}}}
		c.ExposedPorts = map[string]struct{}{"2222/tcp": {}}
	case helperUpload:
		h.Mounts = []mount{{Type: "volume", Source: recordName(id), Target: helperMount}}
	}
	hardened(&h)
	c.HostConfig = h
	return c
}

// writeSSHArchive writes the tar of sshd's two files: the agent's,
// readable by it alone (the entrypoint copies them to tmpfs).
func writeSSHArchive(w io.Writer, spec protocol.EnvironmentSpec) error {
	tw := tar.NewWriter(w)
	now := time.Now()
	files := []struct{ name, data string }{
		{"host_ed25519", spec.SSH.HostPrivateKey},
		{"authorized_keys", strings.TrimSpace(spec.SSH.AuthorizedKey) + "\n"},
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: 0o400, Uid: agentUID, Gid: agentUID, Size: int64(len(f.data)), ModTime: now}); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, f.data); err != nil {
			return err
		}
	}
	return tw.Close()
}

// pullQuery is POST /images/create's query for an image reference: the
// name and the tag or digest, never a bare name (which pulls every tag).
func pullQuery(ref string) url.Values {
	name, tag := ref, "latest"
	if i := strings.Index(name, "@"); i >= 0 {
		name, tag = name[:i], name[i+1:]
		if j := strings.LastIndex(name, ":"); j > strings.LastIndex(name, "/") {
			name = name[:j]
		}
	} else if j := strings.LastIndex(name, ":"); j > strings.LastIndex(name, "/") {
		name, tag = name[:j], name[j+1:]
	}
	return url.Values{"fromImage": {name}, "tag": {tag}}
}

func ptr[T any](v T) *T { return &v }
