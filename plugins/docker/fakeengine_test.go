package docker

import (
	"archive/tar"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine is an in-memory Docker engine, API v1.41, with the
// behaviours the plugin relies on (observed on Docker 29.8): a HostPort
// of "0" is allocated anew at every start; an archive upload into a
// read-only rootfs or a read-only mount is refused; creating a volume
// that exists returns it; a container's anonymous volumes go with
// DELETE ?v=1; a volume in use can't be removed.
type fakeEngine struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer // by name
	volumes    map[string]*fakeVolume
	networks   map[string]*fakeNetwork
	images     map[string]fakeImage // by reference
	nextPort   int
	taken      map[int]bool // ports something else holds
	pullDelay  time.Duration
	pullFails  map[string]string // reference → error
	pulls      int
	// healthy says a started container is healthy at once; false keeps
	// it "starting" until markHealthy.
	healthy bool
	// exits names containers whose process exits as soon as started
	// (a failing entrypoint): "restarting" with a growing RestartCount.
	exits map[string]int
	// takeOnHelperRemove makes the port a removed helper probed taken by
	// something else, as another process might in the gap.
	takeOnHelperRemove bool
	// security is /info's SecurityOptions.
	security                  []string
	apiVersion, minAPIVersion string
	requests                  []string
}

type fakeContainer struct {
	id      string
	name    string
	created time.Time
	cfg     containerCreate
	state   string // created, running, exited, restarting, removing
	health  string
	exit    int
	ports   map[string]int // "2222/tcp" → host port, while running
	restart int
	files   map[string]fakeFile // archive uploads, by full path
	anon    []string            // anonymous volumes
	started time.Time
}

type fakeFile struct {
	data []byte
	mode int64
	uid  int
	gid  int
}

type fakeVolume struct {
	name    string
	labels  map[string]string
	created time.Time
	anon    bool
	files   map[string]fakeFile // uploaded through a container mounting it
}

type fakeNetwork struct {
	id      string
	name    string
	options map[string]string
	labels  map[string]string
	driver  string
}

type fakeImage struct {
	id     string
	digest string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		containers: map[string]*fakeContainer{}, volumes: map[string]*fakeVolume{}, networks: map[string]*fakeNetwork{},
		images:   map[string]fakeImage{"ghcr.io/loomux/agent:test": {id: "sha256:img1", digest: "sha256:aaaa"}},
		nextPort: 32768, taken: map[int]bool{}, pullFails: map[string]string{}, healthy: true, exits: map[string]int{},
		security: []string{"name=seccomp,profile=builtin"}, apiVersion: "1.47", minAPIVersion: "1.24",
	}
}

func (f *fakeEngine) handler() http.Handler {
	mux := http.NewServeMux()
	p := "/" + apiVersion
	mux.HandleFunc("GET "+p+"/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", f.apiVersion)
		io.WriteString(w, "OK")
	})
	mux.HandleFunc("GET "+p+"/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"Version": "29.8.2", "ApiVersion": f.apiVersion, "MinAPIVersion": f.minAPIVersion, "Os": "linux"})
	})
	mux.HandleFunc("GET "+p+"/info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ServerVersion": "29.8.2", "OSType": "linux", "SecurityOptions": f.security, "InitBinary": "docker-init"})
	})
	mux.HandleFunc("GET "+p+"/networks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		filters := parseFilters(r)
		out := []map[string]any{}
		for _, n := range f.networks {
			if names := filters["name"]; len(names) > 0 && !contains(names, n.name) {
				continue
			}
			out = append(out, n.json())
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET "+p+"/networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := f.networks[r.PathValue("name")]
		if n == nil {
			fail(w, 404, "network not found")
			return
		}
		writeJSON(w, 200, n.json())
	})
	mux.HandleFunc("POST "+p+"/networks/create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name    string
			Driver  string
			Options map[string]string
			Labels  map[string]string
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.networks[req.Name]; ok {
			fail(w, 409, "network with name "+req.Name+" already exists")
			return
		}
		f.networks[req.Name] = &fakeNetwork{id: "net" + strconv.Itoa(len(f.networks)+1), name: req.Name, options: req.Options, labels: req.Labels, driver: req.Driver}
		writeJSON(w, 201, map[string]any{"Id": f.networks[req.Name].id, "Warning": ""})
	})
	mux.HandleFunc("GET "+p+"/volumes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		filters := parseFilters(r)
		out := []map[string]any{}
		for _, v := range f.volumes {
			if !matchLabels(v.labels, filters["label"]) {
				continue
			}
			out = append(out, v.json())
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["Name"].(string) < out[j]["Name"].(string) })
		writeJSON(w, 200, map[string]any{"Volumes": out, "Warnings": []string{}})
	})
	mux.HandleFunc("GET "+p+"/volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		v := f.volumes[r.PathValue("name")]
		if v == nil {
			fail(w, 404, "get "+r.PathValue("name")+": no such volume")
			return
		}
		writeJSON(w, 200, v.json())
	})
	mux.HandleFunc("POST "+p+"/volumes/create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name   string
			Labels map[string]string
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		v := f.volumes[req.Name]
		if v == nil {
			v = &fakeVolume{name: req.Name, labels: req.Labels, created: time.Now()}
			f.volumes[req.Name] = v
		}
		writeJSON(w, 201, v.json())
	})
	mux.HandleFunc("DELETE "+p+"/volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		if _, ok := f.volumes[name]; !ok {
			if r.URL.Query().Get("force") == "true" || r.URL.Query().Get("force") == "1" {
				w.WriteHeader(204)
				return
			}
			fail(w, 404, "get "+name+": no such volume")
			return
		}
		for _, c := range f.containers {
			for _, m := range c.cfg.HostConfig.Mounts {
				if m.Source == name {
					fail(w, 409, "remove "+name+": volume is in use - ["+c.id+"]")
					return
				}
			}
		}
		delete(f.volumes, name)
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET "+p+"/images/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		ref := strings.TrimSuffix(r.PathValue("ref"), "/json")
		f.mu.Lock()
		defer f.mu.Unlock()
		img, ok := f.images[ref]
		if !ok {
			for _, i := range f.images {
				if i.id == ref {
					img, ok = i, true
				}
			}
		}
		if !ok {
			fail(w, 404, "No such image: "+ref)
			return
		}
		digests := []string{}
		if img.digest != "" {
			digests = append(digests, strings.SplitN(ref, ":", 2)[0]+"@"+img.digest)
		}
		writeJSON(w, 200, map[string]any{"Id": img.id, "RepoDigests": digests, "RepoTags": []string{ref}})
	})
	mux.HandleFunc("POST "+p+"/images/create", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("fromImage")
		if tag := r.URL.Query().Get("tag"); tag != "" {
			if strings.HasPrefix(tag, "sha256:") {
				ref += "@" + tag
			} else {
				ref += ":" + tag
			}
		}
		f.mu.Lock()
		f.pulls++
		delay, failMsg := f.pullDelay, f.pullFails[ref]
		f.mu.Unlock()
		w.WriteHeader(200)
		io.WriteString(w, `{"status":"Pulling from x","id":"latest"}`+"\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		if failMsg != "" {
			io.WriteString(w, `{"errorDetail":{"message":"`+failMsg+`"},"error":"`+failMsg+`"}`+"\n")
			return
		}
		f.mu.Lock()
		f.images[ref] = fakeImage{id: "sha256:pulled", digest: "sha256:bbbb"}
		f.mu.Unlock()
		io.WriteString(w, `{"status":"Status: Downloaded newer image"}`+"\n")
	})
	mux.HandleFunc("POST "+p+"/containers/create", func(w http.ResponseWriter, r *http.Request) {
		var cfg containerCreate
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			fail(w, 400, err.Error())
			return
		}
		name := r.URL.Query().Get("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.containers[name]; ok {
			fail(w, 409, "Conflict. The container name \"/"+name+"\" is already in use")
			return
		}
		if _, ok := f.images[cfg.Image]; !ok {
			fail(w, 404, "No such image: "+cfg.Image)
			return
		}
		if cfg.HostConfig.NetworkMode != "" && cfg.HostConfig.NetworkMode != "none" && cfg.HostConfig.NetworkMode != "bridge" {
			if _, ok := f.networks[cfg.HostConfig.NetworkMode]; !ok {
				fail(w, 404, "network "+cfg.HostConfig.NetworkMode+" not found")
				return
			}
		}
		c := &fakeContainer{id: fmt.Sprintf("%064x", len(f.containers)+1), name: name, created: time.Now(), cfg: cfg, state: "created", files: map[string]fakeFile{}}
		for _, m := range cfg.HostConfig.Mounts {
			if m.Type == "volume" && m.Source == "" {
				anon := "anon-" + strconv.Itoa(len(f.volumes)+1)
				f.volumes[anon] = &fakeVolume{name: anon, anon: true, created: time.Now()}
				c.anon = append(c.anon, anon)
			} else if m.Type == "volume" {
				if _, ok := f.volumes[m.Source]; !ok {
					f.volumes[m.Source] = &fakeVolume{name: m.Source, created: time.Now()}
				}
			}
		}
		f.containers[name] = c
		writeJSON(w, 201, map[string]any{"Id": c.id, "Warnings": []string{}})
	})
	mux.HandleFunc("GET "+p+"/containers/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		filters := parseFilters(r)
		all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
		out := []map[string]any{}
		for _, c := range f.containers {
			if !all && c.state != "running" {
				continue
			}
			if !matchLabels(c.cfg.Labels, filters["label"]) {
				continue
			}
			out = append(out, map[string]any{"Id": c.id, "Names": []string{"/" + c.name}, "Labels": c.cfg.Labels, "State": c.state, "Created": c.created.Unix()})
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET "+p+"/containers/{name}/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.find(r.PathValue("name"))
		if c == nil {
			fail(w, 404, "No such container: "+r.PathValue("name"))
			return
		}
		if n, ok := f.exits[c.name]; ok && c.state == "running" {
			c.state, c.restart, c.exit = "restarting", n, 1
			f.exits[c.name] = n + 1
		}
		writeJSON(w, 200, c.inspect(f))
	})
	mux.HandleFunc("POST "+p+"/containers/{name}/start", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.find(r.PathValue("name"))
		if c == nil {
			fail(w, 404, "No such container: "+r.PathValue("name"))
			return
		}
		if c.state == "running" || c.state == "restarting" {
			w.WriteHeader(304)
			return
		}
		if msg := f.start(c); msg != "" {
			fail(w, 500, msg)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST "+p+"/containers/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.find(r.PathValue("name"))
		if c == nil {
			fail(w, 404, "No such container: "+r.PathValue("name"))
			return
		}
		if c.state != "running" && c.state != "restarting" {
			w.WriteHeader(304)
			return
		}
		c.state, c.exit, c.ports, c.health = "exited", 0, nil, ""
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE "+p+"/containers/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.find(r.PathValue("name"))
		if c == nil {
			fail(w, 404, "No such container: "+r.PathValue("name"))
			return
		}
		force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
		if c.state == "running" && !force {
			fail(w, 409, "cannot remove a running container")
			return
		}
		if r.URL.Query().Get("v") == "1" || r.URL.Query().Get("v") == "true" {
			for _, a := range c.anon {
				delete(f.volumes, a)
			}
		}
		if f.takeOnHelperRemove && c.cfg.Labels[labelRole] == roleInit && len(c.ports) > 0 {
			for _, p := range c.ports {
				f.taken[p] = true
			}
			f.takeOnHelperRemove = false // once: the gap is a race, not a rule
		}
		delete(f.containers, c.name)
		w.WriteHeader(204)
	})
	mux.HandleFunc("PUT "+p+"/containers/{name}/archive", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.find(r.PathValue("name"))
		if c == nil {
			fail(w, 404, "No such container: "+r.PathValue("name"))
			return
		}
		dir := r.URL.Query().Get("path")
		var vol *fakeVolume
		for _, m := range c.cfg.HostConfig.Mounts {
			if strings.HasPrefix(dir, m.Target) {
				if m.ReadOnly {
					fail(w, 403, "mounted volume is marked read-only")
					return
				}
				vol = f.volumes[m.Source]
			}
		}
		if vol == nil && c.cfg.HostConfig.ReadonlyRootfs {
			fail(w, 403, "container rootfs is marked read-only")
			return
		}
		if vol != nil && vol.files == nil {
			vol.files = map[string]fakeFile{}
		}
		tr := tar.NewReader(r.Body)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			data, _ := io.ReadAll(tr)
			file := fakeFile{data: data, mode: h.Mode, uid: h.Uid, gid: h.Gid}
			if vol != nil {
				vol.files[h.Name] = file
			} else {
				c.files[strings.TrimSuffix(dir, "/")+"/"+h.Name] = file
			}
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("GET "+p+"/containers/{name}/logs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.find(r.PathValue("name"))
		if c == nil {
			fail(w, 404, "No such container: "+r.PathValue("name"))
			return
		}
		w.WriteHeader(200)
		for _, line := range []string{"loomux-agent: sshd starting on port 2222\n", "loomux-agent: /data is not writable by agent (10002)\n"} {
			var hdr [8]byte
			hdr[0] = 2
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(line)))
			w.Write(hdr[:])
			io.WriteString(w, line)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
}

// find is a container by name or id.
func (f *fakeEngine) find(key string) *fakeContainer {
	if c := f.containers[key]; c != nil {
		return c
	}
	for _, c := range f.containers {
		if c.id == key || strings.HasPrefix(c.id, key) {
			return c
		}
	}
	return nil
}

// start allocates the published ports as Docker does: an explicit
// HostPort is bound (refused when taken), "0" gets the next free one.
func (f *fakeEngine) start(c *fakeContainer) string {
	ports := map[string]int{}
	used := map[int]bool{}
	for k := range f.taken {
		used[k] = true
	}
	for _, o := range f.containers {
		for _, p := range o.ports {
			used[p] = true
		}
	}
	for port, bindings := range c.cfg.HostConfig.PortBindings {
		for _, b := range bindings {
			n, _ := strconv.Atoi(b.HostPort)
			if n == 0 {
				for used[f.nextPort] {
					f.nextPort++
				}
				n = f.nextPort
				f.nextPort++
			} else if used[n] {
				return "driver failed programming external connectivity on endpoint " + c.name + ": Bind for " + b.HostIP + ":" + b.HostPort + " failed: port is already allocated"
			}
			ports[port] = n
		}
	}
	c.ports, c.state, c.started = ports, "running", time.Now()
	c.health = "starting"
	if f.healthy {
		c.health = "healthy"
	}
	return ""
}

func (f *fakeEngine) markHealthy(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[name]; c != nil && c.state == "running" {
		c.health = "healthy"
	}
}

// vanish removes a container as someone with docker rm -f would.
func (f *fakeEngine) vanish(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, name)
}

func (f *fakeEngine) container(name string) *fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[name]
}

func (f *fakeEngine) volume(name string) *fakeVolume {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volumes[name]
}

// addVolume puts a volume there as a crash or another instance would
// have left it.
func (f *fakeEngine) addVolume(name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[name] = &fakeVolume{name: name, labels: labels, created: time.Now()}
}

// addContainer puts a container there (running) as a crash would have
// left it.
func (f *fakeEngine) addContainer(name string, cfg containerCreate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeContainer{id: fmt.Sprintf("%064x", 900+len(f.containers)), name: name, created: time.Now(), cfg: cfg, state: "created", files: map[string]fakeFile{}}
	f.containers[name] = c
	f.start(c)
}

func (f *fakeEngine) names() (containers, volumes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n := range f.containers {
		containers = append(containers, n)
	}
	for n := range f.volumes {
		volumes = append(volumes, n)
	}
	sort.Strings(containers)
	sort.Strings(volumes)
	return
}

func (c *fakeContainer) inspect(f *fakeEngine) map[string]any {
	ports := map[string]any{}
	if c.state == "running" || c.state == "restarting" {
		for k, v := range c.ports {
			ports[k] = []map[string]string{{"HostIp": "127.0.0.1", "HostPort": strconv.Itoa(v)}}
		}
	}
	state := map[string]any{"Status": c.state, "Running": c.state == "running", "Restarting": c.state == "restarting", "ExitCode": c.exit, "Error": "", "StartedAt": c.started.UTC().Format(time.RFC3339Nano)}
	if c.cfg.Healthcheck != nil && c.state == "running" {
		state["Health"] = map[string]any{"Status": c.health}
	}
	img := f.images[c.cfg.Image]
	return map[string]any{
		"Id": c.id, "Name": "/" + c.name, "Created": c.created.UTC().Format(time.RFC3339Nano), "Image": img.id, "RestartCount": c.restart,
		"State":  state,
		"Config": map[string]any{"Image": c.cfg.Image, "Labels": c.cfg.Labels, "User": c.cfg.User, "Env": c.cfg.Env},
		"HostConfig": map[string]any{"PortBindings": c.cfg.HostConfig.PortBindings, "ReadonlyRootfs": c.cfg.HostConfig.ReadonlyRootfs,
			"NetworkMode": c.cfg.HostConfig.NetworkMode, "RestartPolicy": c.cfg.HostConfig.RestartPolicy},
		"NetworkSettings": map[string]any{"Ports": ports},
	}
}

func (v *fakeVolume) json() map[string]any {
	return map[string]any{"Name": v.name, "Driver": "local", "Labels": v.labels, "CreatedAt": v.created.UTC().Format(time.RFC3339), "Mountpoint": "/var/lib/docker/volumes/" + v.name + "/_data"}
}

func (n *fakeNetwork) json() map[string]any {
	return map[string]any{"Id": n.id, "Name": n.name, "Driver": n.driver, "Options": n.options, "Labels": n.labels, "Internal": false}
}

func parseFilters(r *http.Request) map[string][]string {
	out := map[string][]string{}
	raw := r.URL.Query().Get("filters")
	if raw == "" {
		return out
	}
	// Either {"label":["a=b"]} or {"label":{"a=b":true}}: the API takes both.
	var list map[string][]string
	if json.Unmarshal([]byte(raw), &list) == nil {
		return list
	}
	var set map[string]map[string]bool
	if json.Unmarshal([]byte(raw), &set) == nil {
		for k, m := range set {
			for v := range m {
				out[k] = append(out[k], v)
			}
		}
	}
	return out
}

func matchLabels(labels map[string]string, wants []string) bool {
	for _, w := range wants {
		k, v, hasValue := strings.Cut(w, "=")
		got, ok := labels[k]
		if !ok || hasValue && got != v {
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// fakePlugin is a configured plugin over a fresh fake engine reached
// through a unix socket, with short waits.
func fakePlugin(t *testing.T, extra map[string]any) (*Plugin, *fakeEngine) {
	t.Helper()
	f := newFakeEngine()
	path := serveUnix(t, f.handler())
	p := newTestPlugin()
	cfg := map[string]any{"engine": "unix://" + path, "bind_address": "127.0.0.1", "ssh_proxy": "none"}
	for k, v := range extra {
		cfg[k] = v
	}
	if err := p.Configure(ctx, configureParams(t, cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { p.Shutdown(ctx) })
	return p, f
}
