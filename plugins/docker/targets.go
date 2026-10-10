package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// The targets.* group (design §1.9, §9, and the plan's findings). The
// record volume lx-<id>-ssh is the machine's record: while it exists
// the machine exists, stopped when its container is gone (a persistent
// one, whose data volume stays) or lost (an ephemeral one). create =
// network, the probed port, the record, its files, the data volume, the
// container, start; stop = stop; start = start, or the container again
// from the record; recreate = the container and the record again with
// the same port and data; destroy = everything, each tolerating 404.

// createOp is one machine's creation in progress, or its outcome, kept
// for OpTTL so targets.get can report why a create failed.
type createOp struct {
	spec protocol.EnvironmentSpec
	done chan struct{}

	mu       sync.Mutex
	phase    string
	err      error
	env      protocol.Environment
	finished time.Time
}

func (o *createOp) setPhase(s string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.phase = s
	o.mu.Unlock()
}

func (o *createOp) state() (phase string, err error, finished time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.phase, o.err, o.finished
}

// mapErr turns what the transport and the engine say into the
// protocol's errors; an rpc error passes through.
func (p *Plugin) mapErr(err error) error {
	var rpcErr *rpc.Error
	var ee *engineError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rpcErr):
		return err
	case errors.Is(err, errHostKeyUnpinned):
		return &rpc.Error{Code: rpc.CodeUnavailable, Message: "the docker host's key isn't pinned: run the check and trust the key it scanned"}
	case errors.Is(err, errHostKeyMismatch):
		return &rpc.Error{Code: rpc.CodeUnavailable, Message: oneLine(err.Error())}
	case errors.Is(err, errAuth):
		return &rpc.Error{Code: rpc.CodeUnauthorized, Message: "the docker host refused the plugin's key"}
	case errors.As(err, &ee):
		if ee.Status == 401 || ee.Status == 403 {
			return &rpc.Error{Code: rpc.CodeUnauthorized, Message: "docker engine: " + ee.Message}
		}
		return &rpc.Error{Code: rpc.CodeUnavailable, Message: "docker engine: " + ee.Message}
	case errors.Is(err, context.DeadlineExceeded):
		return &rpc.Error{Code: rpc.CodeUnavailable, Message: "the docker engine didn't answer in time"}
	}
	return &rpc.Error{Code: rpc.CodeUnavailable, Message: "docker host: " + oneLine(err.Error())}
}

func notFound(id string) error {
	return &rpc.Error{Code: rpc.CodeNotFound, Message: "no machine " + id}
}

// record is a machine's record volume, nil when absent or another
// instance's (invisible to this one).
func (p *Plugin) record(ctx context.Context, eng *engine, host protocol.HostInfo, id string) (*volumeInfo, error) {
	var v volumeInfo
	err := eng.get(ctx, "/volumes/"+recordName(id), nil, &v)
	if isStatus(err, 404) {
		return nil, nil
	}
	if err != nil {
		return nil, p.mapErr(err)
	}
	if v.Labels[protocol.LabelInstance] != host.InstanceID || v.Labels[labelManagedBy] != managedBy {
		return nil, nil
	}
	return &v, nil
}

// records are this instance's machines' records.
func (p *Plugin) records(ctx context.Context, eng *engine, host protocol.HostInfo) ([]volumeInfo, error) {
	var list volumesList
	if err := eng.get(ctx, "/volumes", labelFilter(host.InstanceID, roleAgent), &list); err != nil {
		return nil, p.mapErr(err)
	}
	return list.Volumes, nil
}

// inspect is a container by name, nil when absent.
func (p *Plugin) inspect(ctx context.Context, eng *engine, name string) (*containerInspect, error) {
	var c containerInspect
	err := eng.get(ctx, "/containers/"+name+"/json", nil, &c)
	if isStatus(err, 404) {
		return nil, nil
	}
	if err != nil {
		return nil, p.mapErr(err)
	}
	return &c, nil
}

// ours reports whether labels carry this instance's identity.
func ours(labels map[string]string, host protocol.HostInfo) bool {
	return labels[labelManagedBy] == managedBy && labels[protocol.LabelInstance] == host.InstanceID
}

// removeContainer removes a container and its anonymous volumes,
// running or not; a missing one is fine.
func (p *Plugin) removeContainer(ctx context.Context, eng *engine, name string) error {
	err := eng.delete(ctx, "/containers/"+name, url.Values{"force": {"1"}, "v": {"1"}})
	if err != nil && !isStatus(err, 404) {
		return p.mapErr(err)
	}
	return nil
}

// removeVolume removes a volume; a missing one is fine.
func (p *Plugin) removeVolume(ctx context.Context, eng *engine, name string) error {
	err := eng.delete(ctx, "/volumes/"+name, url.Values{"force": {"true"}})
	if err != nil && !isStatus(err, 404) {
		return p.mapErr(err)
	}
	return nil
}

// ensureVolume is a named volume with labels, made if absent (the
// engine returns an existing one as it is).
func (p *Plugin) ensureVolume(ctx context.Context, eng *engine, name string, labels map[string]string) (*volumeInfo, error) {
	var v volumeInfo
	if err := eng.post(ctx, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, &v); err != nil {
		return nil, p.mapErr(err)
	}
	return &v, nil
}

// ensureNetwork is the agents' bridge, made once per host with
// container-to-container traffic off.
func (p *Plugin) ensureNetwork(ctx context.Context, eng *engine, host protocol.HostInfo) error {
	var n networkInfo
	err := eng.get(ctx, "/networks/"+networkName, nil, &n)
	if err == nil {
		return nil
	}
	if !isStatus(err, 404) {
		return p.mapErr(err)
	}
	err = eng.post(ctx, "/networks/create", nil, map[string]any{
		"Name": networkName, "Driver": "bridge", "CheckDuplicate": true,
		"Options": map[string]string{"com.docker.network.bridge.enable_icc": "false"},
		"Labels":  map[string]string{labelManagedBy: managedBy, protocol.LabelInstance: host.InstanceID},
	}, nil)
	if err != nil && !isStatus(err, 409) {
		return p.mapErr(err)
	}
	return nil
}

// ensureImage pulls the image when the engine doesn't have it. The
// pull's progress stream carries errors as objects of its own.
func (p *Plugin) ensureImage(ctx context.Context, eng *engine, ref string, op *createOp) error {
	err := eng.get(ctx, "/images/"+url.PathEscape(ref)+"/json", nil, nil)
	if err == nil {
		return nil
	}
	if !isStatus(err, 404) {
		return p.mapErr(err)
	}
	op.setPhase("pulling the image " + ref)
	p.Logger.Printf("pulling %s", ref)
	body, err := eng.stream(ctx, "POST", "/images/create", pullQuery(ref))
	if err != nil {
		return p.mapErr(err)
	}
	defer body.Close()
	dec := json.NewDecoder(body)
	for {
		var msg struct {
			Error  string `json:"error"`
			Status string `json:"status"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return &rpc.Error{Code: rpc.CodeUnavailable, Message: "pulling " + ref + ": the engine's answer ended early"}
		}
		if msg.Error != "" {
			return &rpc.Error{Code: rpc.CodeUnavailable, Message: "pulling " + ref + ": " + oneLine(msg.Error)}
		}
	}
}

// probePort learns a free host port the way Docker allocates one: a
// helper publishing sshd's port on an ephemeral host port, read back
// from inspect, then removed. The port is then fixed in the record and
// the container, since an ephemeral one changes at every start.
func (p *Plugin) probePort(ctx context.Context, eng *engine, cfg Config, host protocol.HostInfo, id, image string) (int, error) {
	name := helperName(id)
	if err := p.removeContainer(ctx, eng, name); err != nil {
		return 0, err
	}
	if err := eng.post(ctx, "/containers/create", url.Values{"name": {name}}, helperFor(id, image, cfg, host.InstanceID, helperProbe), nil); err != nil {
		return 0, p.mapErr(err)
	}
	defer func() { _ = p.removeContainer(context.WithoutCancel(ctx), eng, name) }()
	if err := eng.post(ctx, "/containers/"+name+"/start", nil, nil, nil); err != nil {
		return 0, p.mapErr(err)
	}
	c, err := p.inspect(ctx, eng, name)
	if err != nil {
		return 0, err
	}
	if c == nil {
		return 0, &rpc.Error{Code: rpc.CodeUnavailable, Message: "the port probe vanished"}
	}
	port := publishedPort(c)
	if port == 0 {
		return 0, &rpc.Error{Code: rpc.CodeUnavailable, Message: "the engine published no port for the probe on " + cfg.BindAddress}
	}
	return port, nil
}

// publishedPort is the host port sshd is published on, 0 when none.
func publishedPort(c *containerInspect) int {
	for _, b := range c.NetworkSettings.Ports["2222/tcp"] {
		if n := atoi(b.HostPort); n > 0 {
			return n
		}
	}
	for _, b := range c.HostConfig.PortBindings["2222/tcp"] {
		if n := atoi(b.HostPort); n > 0 {
			return n
		}
	}
	return 0
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// uploadFiles writes sshd's two files into the record volume through a
// helper that mounts it read-write and is never started.
func (p *Plugin) uploadFiles(ctx context.Context, eng *engine, cfg Config, host protocol.HostInfo, spec protocol.EnvironmentSpec) error {
	name := helperName(spec.ID)
	if err := p.removeContainer(ctx, eng, name); err != nil {
		return err
	}
	if err := eng.post(ctx, "/containers/create", url.Values{"name": {name}}, helperFor(spec.ID, spec.Image, cfg, host.InstanceID, helperUpload), nil); err != nil {
		return p.mapErr(err)
	}
	defer func() { _ = p.removeContainer(context.WithoutCancel(ctx), eng, name) }()
	var buf bytes.Buffer
	if err := writeSSHArchive(&buf, spec); err != nil {
		return err
	}
	if err := eng.putTar(ctx, "/containers/"+name+"/archive", url.Values{"path": {helperMount}}, &buf); err != nil {
		return p.mapErr(err)
	}
	return nil
}

// ensureContainer is the machine's container, made if absent.
func (p *Plugin) ensureContainer(ctx context.Context, eng *engine, cfg Config, host protocol.HostInfo, spec protocol.EnvironmentSpec, port int) (*containerInspect, error) {
	size, ok := cfg.size(spec.Size)
	if !ok {
		return nil, invalidParams("the machine's size %q is no longer offered; recreate it with one of %s", spec.Size, cfg.sizeNames())
	}
	c, err := p.inspect(ctx, eng, containerName(spec.ID))
	if err != nil || c != nil {
		return c, err
	}
	err = eng.post(ctx, "/containers/create", url.Values{"name": {containerName(spec.ID)}}, containerFor(spec, size, cfg, host.InstanceID, port), nil)
	if err != nil && !isStatus(err, 409) {
		return nil, p.mapErr(err)
	}
	return p.inspect(ctx, eng, containerName(spec.ID))
}

// startContainer starts a container; one already running is fine.
func (p *Plugin) startContainer(ctx context.Context, eng *engine, name string) error {
	if err := eng.post(ctx, "/containers/"+name+"/start", nil, nil, nil); err != nil {
		if portTaken(err) {
			return err
		}
		return p.mapErr(err)
	}
	return nil
}

// build makes a machine from its record on, or from nothing: the
// record (with port, probed unless known), its files, the data volume,
// the container, started. A port found taken at start rebuilds the
// record with a fresh probe.
func (p *Plugin) build(ctx context.Context, eng *engine, cfg Config, host protocol.HostInfo, spec protocol.EnvironmentSpec, port int, created time.Time, op *createOp) (protocol.Environment, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		rec, err := p.record(ctx, eng, host, spec.ID)
		if err != nil {
			return protocol.Environment{}, err
		}
		if rec == nil {
			if port == 0 {
				op.setPhase("choosing a port")
				if port, err = p.probePort(ctx, eng, cfg, host, spec.ID, spec.Image); err != nil {
					return protocol.Environment{}, err
				}
			}
			if rec, err = p.ensureVolume(ctx, eng, recordName(spec.ID), recordLabels(spec, host.InstanceID, port, created)); err != nil {
				return protocol.Environment{}, err
			}
		}
		if port = portFrom(rec.Labels); port == 0 {
			return protocol.Environment{}, &rpc.Error{Code: rpc.CodeInternal, Message: "the machine's record on the docker host is damaged (no port)"}
		}
		created = createdFrom(rec.Labels, created)
		op.setPhase("writing sshd's files")
		if err := p.uploadFiles(ctx, eng, cfg, host, spec); err != nil {
			return protocol.Environment{}, err
		}
		if spec.Persistent {
			labels := labelsFor(spec.ID, spec.TargetID, host.InstanceID)
			labels[labelRole] = "data"
			labels[labelName] = cleanText(spec.Name)
			if _, err := p.ensureVolume(ctx, eng, dataName(spec.ID), labels); err != nil {
				return protocol.Environment{}, err
			}
		}
		op.setPhase("starting the container")
		c, err := p.ensureContainer(ctx, eng, cfg, host, spec, port)
		if err != nil {
			return protocol.Environment{}, err
		}
		err = p.startContainer(ctx, eng, c.Name[1:])
		if portTaken(err) {
			p.Logger.Printf("%s: port %d is taken on the docker host; choosing another", spec.ID, port)
			lastErr = err
			if err := p.removeContainer(ctx, eng, containerName(spec.ID)); err != nil {
				return protocol.Environment{}, err
			}
			if err := p.removeVolume(ctx, eng, recordName(spec.ID)); err != nil {
				return protocol.Environment{}, err
			}
			port = 0
			continue
		}
		if err != nil {
			return protocol.Environment{}, err
		}
		return p.GetTarget(ctx, spec.ID)
	}
	return protocol.Environment{}, &rpc.Error{Code: rpc.CodeUnavailable, Message: "every port the docker host offered was taken before the machine could use it: " + oneLine(lastErr.Error())}
}

// runCreate is the creation, in its own goroutine: the image, the
// network, the quota, then build.
func (p *Plugin) runCreate(cfg Config, eng *engine, host protocol.HostInfo, op *createOp) {
	ctx, cancel := context.WithTimeout(context.Background(), p.CreateTimeout)
	defer cancel()
	env, err := func() (protocol.Environment, error) {
		image := op.spec.Image
		if image == "" {
			image = cfg.AgentImage
		}
		if err := p.ensureImage(ctx, eng, image, op); err != nil {
			return protocol.Environment{}, err
		}
		if err := p.ensureNetwork(ctx, eng, host); err != nil {
			return protocol.Environment{}, err
		}
		recs, err := p.records(ctx, eng, host)
		if err != nil {
			return protocol.Environment{}, err
		}
		n := 0
		for _, r := range recs {
			if r.Labels[protocol.LabelEnvironment] != op.spec.ID {
				n++
			}
		}
		if n >= cfg.MaxEnvironments {
			return protocol.Environment{}, &rpc.Error{Code: rpc.CodeQuota, Message: fmt.Sprintf("this plugin already has its %d machines", cfg.MaxEnvironments)}
		}
		return p.build(ctx, eng, cfg, host, op.spec, 0, p.Now(), op)
	}()
	op.mu.Lock()
	op.env, op.err, op.finished = env, err, p.Now()
	op.mu.Unlock()
	close(op.done)
	if err != nil {
		p.Logger.Printf("creating %s failed: %v", op.spec.ID, err)
		return
	}
	p.mu.Lock()
	if p.ops[op.spec.ID] == op {
		delete(p.ops, op.spec.ID)
	}
	p.mu.Unlock()
}

// op is the creation in progress or recently failed for id, nil when
// none (an outcome older than OpTTL is forgotten).
func (p *Plugin) op(id string) *createOp {
	p.mu.Lock()
	defer p.mu.Unlock()
	op := p.ops[id]
	if op == nil {
		return nil
	}
	if _, _, finished := op.state(); !finished.IsZero() && p.Now().Sub(finished) > p.OpTTL {
		delete(p.ops, id)
		return nil
	}
	return op
}

// startOp joins the creation in progress for the spec's id, or starts
// one (a failed one is retried).
func (p *Plugin) startOp(cfg Config, eng *engine, host protocol.HostInfo, spec protocol.EnvironmentSpec) *createOp {
	p.mu.Lock()
	defer p.mu.Unlock()
	if op := p.ops[spec.ID]; op != nil {
		if _, _, finished := op.state(); finished.IsZero() {
			return op
		}
	}
	op := &createOp{spec: spec, done: make(chan struct{}), phase: "starting"}
	p.ops[spec.ID] = op
	go p.runCreate(cfg, eng, host, op)
	return op
}

func (p *Plugin) DescribeTargets(ctx context.Context) (protocol.TargetsInfo, error) {
	cfg, eng, host, err := p.state()
	if err != nil {
		return protocol.TargetsInfo{}, err
	}
	recs, err := p.records(ctx, eng, host)
	if err != nil {
		return protocol.TargetsInfo{}, err
	}
	return protocol.TargetsInfo{
		Sizes:             cfg.Sizes,
		PersistentDefault: true,
		EgressOptions:     []string{protocol.EgressInternet},
		Image:             cfg.AgentImage,
		MaxEnvironments:   cfg.MaxEnvironments,
		Environments:      len(recs),
		AddressTemplate:   cfg.addressTemplate(),
		SSHProxy:          cfg.SSHProxy,
		User:              "agent",
		Port:              sshPort,
	}, nil
}

func (p *Plugin) CreateTarget(ctx context.Context, spec protocol.EnvironmentSpec) (protocol.Environment, error) {
	cfg, eng, host, err := p.state()
	if err != nil {
		return protocol.Environment{}, err
	}
	if err := validateSpec(spec, cfg, host); err != nil {
		return protocol.Environment{}, err
	}
	// Already made (a retry): make sure it runs and answer at once.
	if p.op(spec.ID) == nil {
		rec, err := p.record(ctx, eng, host, spec.ID)
		if err != nil {
			return protocol.Environment{}, err
		}
		if rec != nil {
			c, err := p.inspect(ctx, eng, containerName(spec.ID))
			if err != nil {
				return protocol.Environment{}, err
			}
			if c != nil {
				if err := p.startContainer(ctx, eng, containerName(spec.ID)); err != nil && !portTaken(err) {
					return protocol.Environment{}, err
				}
				return p.GetTarget(ctx, spec.ID)
			}
		}
	}
	op := p.startOp(cfg, eng, host, spec)
	select {
	case <-op.done:
	case <-time.After(p.CreateWait):
	case <-ctx.Done():
	}
	phase, opErr, finished := op.state()
	if finished.IsZero() {
		return p.pending(spec, cfg, "creating", phase), nil
	}
	if opErr != nil {
		return protocol.Environment{}, p.mapErr(opErr)
	}
	op.mu.Lock()
	env := op.env
	op.mu.Unlock()
	return env, nil
}

// pending is a machine still being made (or whose making failed): no
// record yet, the address its template promised.
func (p *Plugin) pending(spec protocol.EnvironmentSpec, cfg Config, status, reason string) protocol.Environment {
	return protocol.Environment{
		ID: spec.ID, Status: status, Reason: reason, Address: cfg.address(sshPort),
		Size: spec.Size, Persistent: spec.Persistent, Egress: spec.Egress, CreatedAt: p.Now(),
	}
}

// environment is a machine as the host sees it, from its record and
// its container (nil when gone).
func (p *Plugin) environment(ctx context.Context, eng *engine, cfg Config, rec *volumeInfo, c *containerInspect) (protocol.Environment, error) {
	spec, err := specFrom(rec.Labels)
	if err != nil {
		return protocol.Environment{}, err
	}
	env := protocol.Environment{
		ID: spec.ID, Address: cfg.address(portFrom(rec.Labels)), Size: spec.Size, Persistent: spec.Persistent, Egress: spec.Egress,
		CreatedAt: createdFrom(rec.Labels, p.Now()),
	}
	if op := p.op(spec.ID); op != nil && c == nil {
		phase, opErr, finished := op.state()
		switch {
		case finished.IsZero():
			env.Status, env.Reason = protocol.EnvCreating, phase
		case opErr != nil:
			env.Status, env.Reason = protocol.EnvError, oneLine(opErr.Error())
		}
		if env.Status != "" {
			return env, nil
		}
	}
	if c == nil {
		env.Status, env.Reason = protocol.EnvLost, "the container is gone"
		if spec.Persistent {
			var v volumeInfo
			if err := eng.get(ctx, "/volumes/"+dataName(spec.ID), nil, &v); err == nil {
				env.Status, env.Reason = protocol.EnvStopped, "the container is gone; its data is kept"
			}
		}
		return env, nil
	}
	env.Status, env.Reason = statusOf(c)
	env.ImageDigest = p.digest(ctx, eng, c.Image)
	return env, nil
}

func (p *Plugin) GetTarget(ctx context.Context, id string) (protocol.Environment, error) {
	cfg, eng, host, err := p.state()
	if err != nil {
		return protocol.Environment{}, err
	}
	if !idPattern.MatchString(id) {
		return protocol.Environment{}, notFound(id)
	}
	rec, err := p.record(ctx, eng, host, id)
	if err != nil {
		return protocol.Environment{}, err
	}
	if rec == nil {
		if op := p.op(id); op != nil {
			phase, opErr, finished := op.state()
			if finished.IsZero() {
				return p.pending(op.spec, cfg, protocol.EnvCreating, phase), nil
			}
			if opErr != nil {
				return p.pending(op.spec, cfg, protocol.EnvError, oneLine(opErr.Error())), nil
			}
		}
		return protocol.Environment{}, notFound(id)
	}
	c, err := p.inspect(ctx, eng, containerName(id))
	if err != nil {
		return protocol.Environment{}, err
	}
	return p.environment(ctx, eng, cfg, rec, c)
}

func (p *Plugin) ListTargets(ctx context.Context) ([]protocol.Environment, error) {
	cfg, eng, host, err := p.state()
	if err != nil {
		return nil, err
	}
	recs, err := p.records(ctx, eng, host)
	if err != nil {
		return nil, err
	}
	out := []protocol.Environment{}
	for i := range recs {
		id := recs[i].Labels[protocol.LabelEnvironment]
		c, err := p.inspect(ctx, eng, containerName(id))
		if err != nil {
			return nil, err
		}
		env, err := p.environment(ctx, eng, cfg, &recs[i], c)
		if err != nil {
			p.Logger.Printf("skipping %s: %v", recs[i].Name, err)
			continue
		}
		out = append(out, env)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (p *Plugin) StartTarget(ctx context.Context, id string) error {
	cfg, eng, host, err := p.state()
	if err != nil {
		return err
	}
	if !idPattern.MatchString(id) {
		return notFound(id)
	}
	rec, err := p.record(ctx, eng, host, id)
	if err != nil {
		return err
	}
	if rec == nil {
		return notFound(id)
	}
	c, err := p.inspect(ctx, eng, containerName(id))
	if err != nil {
		return err
	}
	if c == nil {
		// Gone: a persistent machine with its data is made again from
		// the record; an ephemeral one is lost, and the host creates it
		// anew.
		spec, err := specFrom(rec.Labels)
		if err != nil {
			return err
		}
		if !spec.Persistent {
			return notFound(id)
		}
		if err := eng.get(ctx, "/volumes/"+dataName(id), nil, nil); err != nil {
			if isStatus(err, 404) {
				return notFound(id)
			}
			return p.mapErr(err)
		}
		if c, err = p.ensureContainer(ctx, eng, cfg, host, spec, portFrom(rec.Labels)); err != nil {
			return err
		}
	}
	if err := p.startContainer(ctx, eng, c.Name[1:]); err != nil {
		if portTaken(err) {
			return &rpc.Error{Code: rpc.CodeUnavailable, Message: "the machine's port is in use on the docker host right now; try again shortly"}
		}
		return err
	}
	return nil
}

func (p *Plugin) StopTarget(ctx context.Context, id string) error {
	_, eng, host, err := p.state()
	if err != nil {
		return err
	}
	if !idPattern.MatchString(id) {
		return notFound(id)
	}
	rec, err := p.record(ctx, eng, host, id)
	if err != nil {
		return err
	}
	if rec == nil {
		return notFound(id)
	}
	err = eng.post(ctx, "/containers/"+containerName(id)+"/stop", url.Values{"t": {"30"}}, nil, nil)
	if err != nil && !isStatus(err, 404) {
		return p.mapErr(err)
	}
	return nil
}

func (p *Plugin) RecreateTarget(ctx context.Context, id string, spec protocol.EnvironmentSpec) (protocol.Environment, error) {
	cfg, eng, host, err := p.state()
	if err != nil {
		return protocol.Environment{}, err
	}
	if spec.ID == "" {
		spec.ID = id
	}
	if spec.ID != id {
		return protocol.Environment{}, invalidParams("the spec's id differs from the machine's")
	}
	if err := validateSpec(spec, cfg, host); err != nil {
		return protocol.Environment{}, err
	}
	rec, err := p.record(ctx, eng, host, id)
	if err != nil {
		return protocol.Environment{}, err
	}
	if rec == nil {
		return protocol.Environment{}, notFound(id)
	}
	old, err := specFrom(rec.Labels)
	if err != nil {
		return protocol.Environment{}, err
	}
	if spec.Persistent != old.Persistent {
		return protocol.Environment{}, invalidParams("a machine can't change between persistent and ephemeral")
	}
	image := spec.Image
	if image == "" {
		image = cfg.AgentImage
	}
	if err := p.ensureImage(ctx, eng, image, nil); err != nil {
		return protocol.Environment{}, err
	}
	port, created := portFrom(rec.Labels), createdFrom(rec.Labels, p.Now())
	// The container goes (an ephemeral machine's scratch data with it),
	// then the record, remade with the new spec at the same port; the
	// data volume stays.
	if err := p.removeContainer(ctx, eng, containerName(id)); err != nil {
		return protocol.Environment{}, err
	}
	if err := p.removeContainer(ctx, eng, helperName(id)); err != nil {
		return protocol.Environment{}, err
	}
	if err := p.removeVolume(ctx, eng, recordName(id)); err != nil {
		return protocol.Environment{}, err
	}
	return p.build(ctx, eng, cfg, host, spec, port, created, nil)
}

func (p *Plugin) DestroyTarget(ctx context.Context, id string) error {
	_, eng, host, err := p.state()
	if err != nil {
		return err
	}
	if !idPattern.MatchString(id) {
		return nil
	}
	p.mu.Lock()
	delete(p.ops, id)
	p.mu.Unlock()
	for _, name := range []string{containerName(id), helperName(id)} {
		c, err := p.inspect(ctx, eng, name)
		if err != nil {
			return err
		}
		if c != nil && ours(c.Config.Labels, host) {
			if err := p.removeContainer(ctx, eng, name); err != nil {
				return err
			}
		}
	}
	// The data volume first, the record last: a crash in between leaves
	// a record the host's reconcile destroys again.
	for _, name := range []string{dataName(id), recordName(id)} {
		var v volumeInfo
		err := eng.get(ctx, "/volumes/"+name, nil, &v)
		if isStatus(err, 404) {
			continue
		}
		if err != nil {
			return p.mapErr(err)
		}
		if !ours(v.Labels, host) {
			continue
		}
		if err := p.removeVolume(ctx, eng, name); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) TargetHealth(ctx context.Context, id string) (protocol.EnvironmentHealth, error) {
	cfg, eng, host, err := p.state()
	if err != nil {
		return protocol.EnvironmentHealth{}, err
	}
	if !idPattern.MatchString(id) {
		return protocol.EnvironmentHealth{}, notFound(id)
	}
	rec, err := p.record(ctx, eng, host, id)
	if err != nil {
		return protocol.EnvironmentHealth{}, err
	}
	if rec == nil {
		return protocol.EnvironmentHealth{}, notFound(id)
	}
	c, err := p.inspect(ctx, eng, containerName(id))
	if err != nil {
		return protocol.EnvironmentHealth{}, err
	}
	env, err := p.environment(ctx, eng, cfg, rec, c)
	if err != nil {
		return protocol.EnvironmentHealth{}, err
	}
	h := protocol.EnvironmentHealth{Status: env.Status, Reason: env.Reason, ImageDigest: env.ImageDigest}
	if c != nil {
		h.Restarts = c.RestartCount
		if h.Status == protocol.EnvError {
			if tail := p.logTail(ctx, eng, containerName(id)); tail != "" {
				h.Reason = truncate(h.Reason+" ("+tail+")", 500)
			}
		}
	}
	return h, nil
}

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// attachCommand is how a person attaches to a machine's tmux: docker
// exec on the docker host, named by its engine address for an ssh
// engine.
func attachCommand(cfg Config, id, session string) string {
	cmd := "docker"
	if cfg.Engine.Kind == engineSSH {
		cmd += " -H " + cfg.Engine.String()
	}
	return cmd + " exec -it " + containerName(id) + " tmux -L loomux attach -t " + session
}

func (p *Plugin) TargetAttachCommands(ctx context.Context, id, session string) ([]protocol.AttachCommand, error) {
	cfg, eng, host, err := p.state()
	if err != nil {
		return nil, err
	}
	if !sessionPattern.MatchString(session) {
		return nil, invalidParams("session must be letters, digits, dots, dashes and underscores")
	}
	if !idPattern.MatchString(id) {
		return nil, notFound(id)
	}
	rec, err := p.record(ctx, eng, host, id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, notFound(id)
	}
	return []protocol.AttachCommand{{Via: "docker", Command: attachCommand(cfg, id, session)}}, nil
}
