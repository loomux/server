package docker

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var rpcErr *rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// One persistent machine through its life (design §9's lifecycle
// mapping, with the plan's findings): create makes the network, the
// record with the probed port, the data volume and the container;
// stop keeps the port; start brings it back; recreate keeps address
// and data; destroy leaves nothing.
func TestLifecycle(t *testing.T) {
	p, f := fakePlugin(t, nil)
	info, err := p.DescribeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Sizes) != 3 || !info.PersistentDefault || strings.Join(info.EgressOptions, ",") != protocol.EgressInternet || info.Image != "ghcr.io/loomux/agent:test" || info.MaxEnvironments != 5 || info.Environments != 0 {
		t.Errorf("describe = %+v", info)
	}
	if info.AddressTemplate != "127.0.0.1" || info.SSHProxy != protocol.SSHProxyNone || info.User != "agent" || info.Port != 2222 {
		t.Errorf("describe address = %+v", info)
	}

	sp := spec("k7f3q2")
	env, err := p.CreateTarget(ctx, sp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.ID != "k7f3q2" || env.Status != protocol.EnvRunning || env.Address != (protocol.Address{Host: "127.0.0.1", Port: 32768, Proxy: protocol.SSHProxyNone}) || !env.Persistent || env.Size != "small" || env.Egress != protocol.EgressInternet || env.CreatedAt.IsZero() {
		t.Errorf("created = %+v", env)
	}
	if env.ImageDigest != "sha256:aaaa" {
		t.Errorf("digest = %q", env.ImageDigest)
	}
	c := f.container("lx-k7f3q2")
	if c == nil || c.state != "running" || c.cfg.HostConfig.PortBindings["2222/tcp"][0].HostPort != "32768" || c.ports["2222/tcp"] != 32768 {
		t.Fatalf("container = %+v", c)
	}
	if f.container("lx-k7f3q2-init") != nil {
		t.Error("the helper was left behind")
	}
	rec := f.volume("lx-k7f3q2-ssh")
	if rec == nil || rec.labels[labelPort] != "32768" || rec.labels[protocol.LabelInstance] != testInstance || rec.labels[labelRole] != roleAgent {
		t.Fatalf("record = %+v", rec)
	}
	if file := rec.files["host_ed25519"]; string(file.data) != sp.SSH.HostPrivateKey || file.mode != 0o400 || file.uid != 10002 {
		t.Errorf("record host key file = %+v", file)
	}
	if file := rec.files["authorized_keys"]; !strings.HasPrefix(string(file.data), "no-port-forwarding") {
		t.Errorf("record authorized_keys = %+v", file)
	}
	if f.volume("lx-k7f3q2-data") == nil || f.volume("lx-k7f3q2-data").labels[protocol.LabelEnvironment] != "k7f3q2" {
		t.Error("no labelled data volume")
	}
	f.mu.Lock()
	net := f.networks[networkName]
	f.mu.Unlock()
	if net == nil || net.options["com.docker.network.bridge.enable_icc"] != "false" || net.driver != "bridge" {
		t.Errorf("network = %+v", net)
	}

	again, err := p.CreateTarget(ctx, sp)
	if err != nil || again.Address != env.Address || again.Status != protocol.EnvRunning {
		t.Errorf("create again = %+v, %v", again, err)
	}
	if f.pulls != 0 {
		t.Errorf("the image was there, yet %d pulls", f.pulls)
	}
	got, err := p.GetTarget(ctx, "k7f3q2")
	if err != nil || got.Address != env.Address || got.Status != protocol.EnvRunning {
		t.Errorf("get = %+v, %v", got, err)
	}
	list, err := p.ListTargets(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "k7f3q2" || list[0].Address != env.Address {
		t.Errorf("list = %+v, %v", list, err)
	}
	if info, _ := p.DescribeTargets(ctx); info.Environments != 1 {
		t.Errorf("environments = %d", info.Environments)
	}
	h, err := p.TargetHealth(ctx, "k7f3q2")
	if err != nil || h.Status != protocol.EnvRunning || h.Restarts != 0 || h.ImageDigest != "sha256:aaaa" {
		t.Errorf("health = %+v, %v", h, err)
	}

	if err := p.StopTarget(ctx, "k7f3q2"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if c := f.container("lx-k7f3q2"); c.state != "exited" {
		t.Errorf("after stop the container is %s", c.state)
	}
	got, _ = p.GetTarget(ctx, "k7f3q2")
	if got.Status != protocol.EnvStopped || got.Address.Port != 32768 {
		t.Errorf("stopped keeps the port: %+v", got)
	}
	if err := p.StopTarget(ctx, "k7f3q2"); err != nil {
		t.Errorf("stop twice: %v", err)
	}
	if err := p.StartTarget(ctx, "k7f3q2"); err != nil {
		t.Fatalf("start: %v", err)
	}
	got, _ = p.GetTarget(ctx, "k7f3q2")
	if got.Status != protocol.EnvRunning || got.Address.Port != 32768 {
		t.Errorf("started at the same port: %+v", got)
	}
	if err := p.StartTarget(ctx, "k7f3q2"); err != nil {
		t.Errorf("start twice: %v", err)
	}

	sp2 := sp
	sp2.Size, sp2.Image = "medium", "ghcr.io/loomux/agent:test"
	re, err := p.RecreateTarget(ctx, "k7f3q2", sp2)
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if re.Address != env.Address || re.Size != "medium" || re.Status != protocol.EnvRunning {
		t.Errorf("recreated = %+v", re)
	}
	if c := f.container("lx-k7f3q2"); c.cfg.HostConfig.Memory != 4<<30 || c.cfg.HostConfig.PortBindings["2222/tcp"][0].HostPort != "32768" {
		t.Errorf("recreated container = %+v", c.cfg.HostConfig)
	}
	if rec := f.volume("lx-k7f3q2-ssh"); rec == nil || !strings.Contains(rec.labels[labelSpec], `"size":"medium"`) || rec.labels[labelPort] != "32768" {
		t.Errorf("recreated record = %+v", rec)
	}
	if f.volume("lx-k7f3q2-data") == nil {
		t.Error("recreate lost the data volume")
	}
	sp3 := sp
	sp3.Persistent = false
	_, err = p.RecreateTarget(ctx, "k7f3q2", sp3)
	wantCode(t, err, rpc.CodeInvalidParams)

	cmds, err := p.TargetAttachCommands(ctx, "k7f3q2", "loomux-x")
	if err != nil || len(cmds) != 1 || cmds[0].Via != "docker" || cmds[0].Command != "docker exec -it lx-k7f3q2 tmux -L loomux attach -t loomux-x" {
		t.Errorf("attach = %+v, %v", cmds, err)
	}
	_, err = p.TargetAttachCommands(ctx, "k7f3q2", "bad session;rm")
	wantCode(t, err, rpc.CodeInvalidParams)

	if err := p.DestroyTarget(ctx, "k7f3q2"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	containers, volumes := f.names()
	if len(containers) != 0 || len(volumes) != 0 {
		t.Errorf("after destroy: containers %v volumes %v", containers, volumes)
	}
	_, err = p.GetTarget(ctx, "k7f3q2")
	wantCode(t, err, rpc.CodeNotFound)
	if err := p.DestroyTarget(ctx, "k7f3q2"); err != nil {
		t.Errorf("destroy twice: %v", err)
	}
	if list, _ := p.ListTargets(ctx); len(list) != 0 {
		t.Errorf("list after destroy = %+v", list)
	}
}

func TestAttachCommandForSSHEngine(t *testing.T) {
	cfg, err := parseConfig(map[string]any{"engine": "ssh://docker@jet01.example:2200", "ssh_private_key": firstTestKey(t), "bind_address": "100.64.0.5", "agent_image": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := attachCommand(cfg, "k7f3q2", "loomux-x"); got != "docker -H ssh://docker@jet01.example:2200 exec -it lx-k7f3q2 tmux -L loomux attach -t loomux-x" {
		t.Errorf("attach = %q", got)
	}
}

func firstTestKey(t *testing.T) string {
	k, _ := testKey(t)
	return k
}

// An ephemeral machine's data is an anonymous volume that goes with
// the container; a lost ephemeral container is lost.
func TestEphemeral(t *testing.T) {
	p, f := fakePlugin(t, nil)
	sp := spec("eph1")
	sp.Persistent = false
	env, err := p.CreateTarget(ctx, sp)
	if err != nil || env.Persistent || env.Status != protocol.EnvRunning {
		t.Fatalf("create = %+v, %v", env, err)
	}
	if f.volume("lx-eph1-data") != nil {
		t.Error("an ephemeral machine got a named data volume")
	}
	if c := f.container("lx-eph1"); len(c.anon) != 1 {
		t.Errorf("anonymous volumes = %v", c.anon)
	}
	f.vanish("lx-eph1")
	got, err := p.GetTarget(ctx, "eph1")
	if err != nil || got.Status != protocol.EnvLost {
		t.Errorf("after the container vanished: %+v, %v", got, err)
	}
	err = p.StartTarget(ctx, "eph1")
	wantCode(t, err, rpc.CodeNotFound)
	if err := p.DestroyTarget(ctx, "eph1"); err != nil {
		t.Fatal(err)
	}
	// The record goes; the anonymous volume a hand docker rm (without
	// -v) left behind carries no label and isn't the plugin's to find.
	if _, volumes := f.names(); strings.Contains(strings.Join(volumes, ","), "lx-") {
		t.Errorf("volumes left: %v", volumes)
	}

	// Destroyed through the plugin, the anonymous volume goes too.
	if _, err := p.CreateTarget(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if err := p.DestroyTarget(ctx, "eph1"); err != nil {
		t.Fatal(err)
	}
	if _, volumes := f.names(); len(volumes) != 1 {
		t.Errorf("volumes after a plugin destroy: %v (only the hand-removed one should remain)", volumes)
	}
}

// A persistent machine whose container is gone (docker rm by hand, a
// host reinstall keeping /var/lib/docker/volumes) is stopped, and
// start makes the container again from the record, at its port.
func TestLostPersistentContainer(t *testing.T) {
	p, f := fakePlugin(t, nil)
	if _, err := p.CreateTarget(ctx, spec("k7f3q2")); err != nil {
		t.Fatal(err)
	}
	f.vanish("lx-k7f3q2")
	got, err := p.GetTarget(ctx, "k7f3q2")
	if err != nil || got.Status != protocol.EnvStopped || got.Address.Port != 32768 {
		t.Errorf("get = %+v, %v", got, err)
	}
	h, err := p.TargetHealth(ctx, "k7f3q2")
	if err != nil || h.Status != protocol.EnvStopped {
		t.Errorf("health = %+v, %v", h, err)
	}
	if err := p.StartTarget(ctx, "k7f3q2"); err != nil {
		t.Fatalf("start: %v", err)
	}
	c := f.container("lx-k7f3q2")
	if c == nil || c.state != "running" || c.ports["2222/tcp"] != 32768 || c.cfg.HostConfig.Memory != 2<<30 {
		t.Errorf("remade container = %+v", c)
	}
	if f.container("lx-k7f3q2-init") != nil {
		t.Error("start probed a port it already knew")
	}
	// With the data volume gone too, there is nothing to start: the
	// host creates it again.
	f.vanish("lx-k7f3q2")
	f.mu.Lock()
	delete(f.volumes, "lx-k7f3q2-data")
	f.mu.Unlock()
	if got, _ := p.GetTarget(ctx, "k7f3q2"); got.Status != protocol.EnvLost {
		t.Errorf("without its data: %+v", got)
	}
	wantCode(t, p.StartTarget(ctx, "k7f3q2"), rpc.CodeNotFound)
}

// A create interrupted after the record was made (a crash) is finished
// by the next create: the record's port is kept, the files are written
// again, a stale helper is removed first.
func TestCreateResumes(t *testing.T) {
	p, f := fakePlugin(t, nil)
	sp := spec("k7f3q2")
	cfg := testConfig(t)
	f.addVolume("lx-k7f3q2-ssh", recordLabels(sp, testInstance, 40000, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)))
	f.addContainer("lx-k7f3q2-init", helperFor("k7f3q2", sp.Image, cfg, testInstance, helperProbe))
	env, err := p.CreateTarget(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if env.Address.Port != 40000 || env.Status != protocol.EnvRunning || !env.CreatedAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("resumed = %+v", env)
	}
	if rec := f.volume("lx-k7f3q2-ssh"); len(rec.files) != 2 {
		t.Errorf("record files = %v", rec.files)
	}
	if f.container("lx-k7f3q2-init") != nil {
		t.Error("the stale helper is still there")
	}
	if c := f.container("lx-k7f3q2"); c == nil || c.cfg.HostConfig.PortBindings["2222/tcp"][0].HostPort != "40000" {
		t.Errorf("container = %+v", c)
	}
}

// A port taken between the probe and the start: the record is rebuilt
// with a fresh probe instead of failing forever.
func TestCreateRetriesTakenPort(t *testing.T) {
	p, f := fakePlugin(t, nil)
	f.takeOnHelperRemove = true
	env, err := p.CreateTarget(ctx, spec("k7f3q2"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.Address.Port != 32769 || env.Status != protocol.EnvRunning {
		t.Errorf("after a taken port: %+v", env)
	}
	if rec := f.volume("lx-k7f3q2-ssh"); rec.labels[labelPort] != "32769" {
		t.Errorf("record port = %s", rec.labels[labelPort])
	}
	containers, volumes := f.names()
	if strings.Join(containers, ",") != "lx-k7f3q2" || strings.Join(volumes, ",") != "lx-k7f3q2-data,lx-k7f3q2-ssh" {
		t.Errorf("objects: %v %v", containers, volumes)
	}
}

func TestCreateQuota(t *testing.T) {
	p, _ := fakePlugin(t, map[string]any{"max_environments": float64(1)})
	if _, err := p.CreateTarget(ctx, spec("one")); err != nil {
		t.Fatal(err)
	}
	_, err := p.CreateTarget(ctx, spec("two"))
	wantCode(t, err, rpc.CodeQuota)
	// The same machine again isn't a new one.
	if _, err := p.CreateTarget(ctx, spec("one")); err != nil {
		t.Errorf("create of the existing machine under quota: %v", err)
	}
}

// A missing image is pulled; a slow pull is reported as creating and
// finished in the background; a failed pull is an error.
func TestCreatePullsImage(t *testing.T) {
	t.Run("pulled", func(t *testing.T) {
		p, f := fakePlugin(t, map[string]any{"agent_image": "ghcr.io/loomux/agent:v9"})
		env, err := p.CreateTarget(ctx, specWithImage("pull1", "ghcr.io/loomux/agent:v9"))
		if err != nil || env.Status != protocol.EnvRunning || f.pulls != 1 || env.ImageDigest != "sha256:bbbb" {
			t.Errorf("env = %+v, %v, pulls %d", env, err, f.pulls)
		}
	})
	t.Run("slow", func(t *testing.T) {
		p, f := fakePlugin(t, map[string]any{"agent_image": "ghcr.io/loomux/agent:v9"})
		p.CreateWait = 100 * time.Millisecond
		f.pullDelay = 600 * time.Millisecond
		env, err := p.CreateTarget(ctx, specWithImage("pull2", "ghcr.io/loomux/agent:v9"))
		if err != nil || env.Status != protocol.EnvCreating || !strings.Contains(env.Reason, "pulling") || env.Address.Port != sshPort {
			t.Fatalf("env = %+v, %v", env, err)
		}
		got, err := p.GetTarget(ctx, "pull2")
		if err != nil || got.Status != protocol.EnvCreating {
			t.Errorf("get while pulling = %+v, %v", got, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for got.Status != protocol.EnvRunning && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			got, _ = p.GetTarget(ctx, "pull2")
		}
		if got.Status != protocol.EnvRunning || got.Address.Port != 32768 {
			t.Errorf("after the pull = %+v", got)
		}
	})
	t.Run("fails", func(t *testing.T) {
		p, f := fakePlugin(t, map[string]any{"agent_image": "ghcr.io/loomux/agent:v9"})
		f.pullFails["ghcr.io/loomux/agent:v9"] = "manifest unknown"
		_, err := p.CreateTarget(ctx, specWithImage("pull3", "ghcr.io/loomux/agent:v9"))
		var rpcErr *rpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != rpc.CodeUnavailable || !strings.Contains(rpcErr.Message, "manifest unknown") {
			t.Fatalf("want the pull's error, got %v", err)
		}
		got, err := p.GetTarget(ctx, "pull3")
		if err != nil || got.Status != protocol.EnvError || !strings.Contains(got.Reason, "manifest unknown") {
			t.Errorf("get after a failed create = %+v, %v", got, err)
		}
		// A new create tries again.
		delete(f.pullFails, "ghcr.io/loomux/agent:v9")
		if env, err := p.CreateTarget(ctx, specWithImage("pull3", "ghcr.io/loomux/agent:v9")); err != nil || env.Status != protocol.EnvRunning {
			t.Errorf("retry = %+v, %v", env, err)
		}
	})
}

func specWithImage(id, image string) protocol.EnvironmentSpec {
	s := spec(id)
	s.Image = image
	return s
}

// Another server's machines (another instance label) are invisible:
// never listed, got or destroyed.
func TestOtherInstanceInvisible(t *testing.T) {
	p, f := fakePlugin(t, nil)
	theirs := spec("theirs")
	f.addVolume("lx-theirs-ssh", recordLabels(theirs, "another-instance", 41000, time.Now()))
	f.addContainer("lx-theirs", containerFor(theirs, defaultSizes[0], testConfig(t), "another-instance", 41000))
	if list, err := p.ListTargets(ctx); err != nil || len(list) != 0 {
		t.Errorf("list = %+v, %v", list, err)
	}
	_, err := p.GetTarget(ctx, "theirs")
	wantCode(t, err, rpc.CodeNotFound)
	if err := p.DestroyTarget(ctx, "theirs"); err != nil {
		t.Errorf("destroy: %v", err)
	}
	if f.container("lx-theirs") == nil || f.volume("lx-theirs-ssh") == nil {
		t.Error("another instance's objects were destroyed")
	}
	sp := spec("mine")
	sp.Labels[protocol.LabelInstance] = "another-instance"
	_, err = p.CreateTarget(ctx, sp)
	wantCode(t, err, rpc.CodeInvalidParams)
}

func TestStatusMapping(t *testing.T) {
	mk := func(status, health string, restarts, exit int) *containerInspect {
		var c containerInspect
		c.State.Status, c.State.ExitCode, c.RestartCount = status, exit, restarts
		c.State.Running = status == "running"
		if health != "" {
			c.State.Health = &struct {
				Status string `json:"Status"`
			}{Status: health}
		}
		return &c
	}
	cases := []struct {
		in     *containerInspect
		status string
		reason string
	}{
		{mk("running", "healthy", 0, 0), protocol.EnvRunning, ""},
		{mk("running", "starting", 0, 0), protocol.EnvStarting, "sshd isn't answering yet"},
		{mk("running", "", 0, 0), protocol.EnvStarting, "sshd isn't answering yet"},
		{mk("running", "unhealthy", 0, 0), protocol.EnvError, "sshd isn't answering"},
		{mk("created", "", 0, 0), protocol.EnvCreating, "not started yet"},
		{mk("restarting", "", 0, 1), protocol.EnvStarting, "restarting"},
		{mk("restarting", "", 3, 1), protocol.EnvError, "keeps exiting (exit 1)"},
		{mk("exited", "", 0, 0), protocol.EnvStopped, ""},
		{mk("exited", "", 0, 137), protocol.EnvStopped, "exit 137"},
		{mk("paused", "", 0, 0), protocol.EnvStopped, "paused"},
		{mk("removing", "", 0, 0), protocol.EnvDestroying, ""},
		{mk("dead", "", 0, 0), protocol.EnvError, "dead"},
	}
	for _, c := range cases {
		status, reason := statusOf(c.in)
		if status != c.status || !strings.Contains(reason, c.reason) {
			t.Errorf("statusOf(%s/%v restarts %d exit %d) = %s %q, want %s %q", c.in.State.Status, c.in.State.Health, c.in.RestartCount, c.in.State.ExitCode, status, reason, c.status, c.reason)
		}
	}
}

// A container that keeps exiting is an error whose reason carries the
// container's last log line, the entrypoint's complaint.
func TestHealthWithLogTail(t *testing.T) {
	p, f := fakePlugin(t, nil)
	if _, err := p.CreateTarget(ctx, spec("k7f3q2")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.exits["lx-k7f3q2"] = 3
	f.mu.Unlock()
	h, err := p.TargetHealth(ctx, "k7f3q2")
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != protocol.EnvError || !strings.Contains(h.Reason, "/data is not writable") || h.Restarts < 3 {
		t.Errorf("health = %+v", h)
	}
	if _, err := p.TargetHealth(ctx, "never"); err == nil {
		t.Error("health of an unknown machine")
	}
}

func TestNotConfigured(t *testing.T) {
	p := newTestPlugin()
	_, err := p.DescribeTargets(ctx)
	wantCode(t, err, rpc.CodeUnavailable)
	_, err = p.Check(ctx)
	wantCode(t, err, rpc.CodeUnavailable)
}
