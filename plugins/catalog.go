package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/registry"
)

// ManifestFile is the manifest's file name in a plugin's directory.
const ManifestFile = "plugin.json"

// ExecutableName is the executable's name in a plugin's directory.
func ExecutableName(name string) string { return "loomux-plugin-" + name }

// Isolation values (Available.Isolation): what separates the plugin from
// loomuxd's process.
const (
	// IsolationNone: a subprocess, loomuxd's own user; it could read
	// loomuxd's database and environment.
	IsolationNone = "none"
	// IsolationContainer: a sidecar reached over a socket; its own user
	// and mounts.
	IsolationContainer = "container"
)

// describeTimeout bounds the describe of a socket plugin while listing.
const describeTimeout = 5 * time.Second

// Available is a plugin the catalog found (installed or not).
type Available struct {
	Manifest Manifest
	Source   registry.PluginSource
	// Path is the plugin's directory, or its socket.
	Path      string
	Trust     string
	Isolation string
}

// ErrNotAvailable is a plugin the catalog doesn't have.
var ErrNotAvailable = errors.New("plugins: not available")

// Verifier decides the trust of a plugin directory that isn't bundled:
// the LOOM-118 attestation verifier, once plugin artifacts are signed
// (design §1.6). Nil means every such plugin is unsigned.
type Verifier interface {
	// Verify returns the trust value for dir: registry.PluginTrustUnsigned,
	// or registry.PluginTrustSignedPrefix + the identity that signed it.
	Verify(ctx context.Context, dir string) (string, error)
}

// Catalog finds plugins in the three places the host looks (design
// §1.8): the image's bundle directory, the operator's plugin directory,
// and the socket directory where sidecars listen. An empty directory
// setting is skipped.
type Catalog struct {
	BundleDir string
	PluginDir string
	SocketDir string
	Verifier  Verifier
	Logger    *slog.Logger
}

func (c *Catalog) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}

// Available lists every plugin found, bundled first, then the plugin
// directory's, then sockets, each group by name. An entry that can't be
// read (a bad manifest, a missing executable, a socket nobody answers
// on) is logged and skipped; the listing itself only fails when a
// configured directory can't be read.
func (c *Catalog) Available(ctx context.Context) ([]Available, error) {
	var out []Available
	bundled, err := c.scanDir(ctx, c.BundleDir, registry.PluginSourceBundled)
	if err != nil {
		return nil, err
	}
	out = append(out, bundled...)
	dir, err := c.scanDir(ctx, c.PluginDir, registry.PluginSourceDir)
	if err != nil {
		return nil, err
	}
	out = append(out, dir...)
	sockets, err := c.scanSockets(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, sockets...)
	return out, nil
}

// Find returns the plugin called name from source.
func (c *Catalog) Find(ctx context.Context, name string, source registry.PluginSource) (*Available, error) {
	all, err := c.Available(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Manifest.Name == name && all[i].Source == source {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("%w: plugin %q from %s", ErrNotAvailable, name, source)
}

// scanDir reads root's subdirectories as plugins.
func (c *Catalog) scanDir(ctx context.Context, root string, source registry.PluginSource) ([]Available, error) {
	if root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("plugins: %s directory: %w", source, err)
	}
	var out []Available
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		a, err := c.readDir(ctx, dir, source)
		if err != nil {
			c.logger().Warn("plugin skipped", "source", string(source), "dir", dir, "error", err.Error())
			continue
		}
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out, nil
}

// readDir reads one plugin directory: its manifest, its executable, its
// trust.
func (c *Catalog) readDir(ctx context.Context, dir string, source registry.PluginSource) (*Available, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(data)
	if err != nil {
		return nil, err
	}
	if m.Name != filepath.Base(dir) {
		return nil, fmt.Errorf("plugins: manifest %q in a directory named %q", m.Name, filepath.Base(dir))
	}
	exe := filepath.Join(dir, ExecutableName(m.Name))
	fi, err := os.Stat(exe)
	if err != nil {
		return nil, fmt.Errorf("plugins: %s: %w", ExecutableName(m.Name), err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("plugins: %s isn't an executable file", exe)
	}
	trust := registry.PluginTrustUnsigned
	switch {
	case source == registry.PluginSourceBundled:
		trust = registry.PluginTrustBundled
	case c.Verifier != nil:
		trust, err = c.Verifier.Verify(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("plugins: verify: %w", err)
		}
	}
	return &Available{Manifest: *m, Source: source, Path: dir, Trust: trust, Isolation: IsolationNone}, nil
}

// scanSockets lists the sockets in SocketDir and asks each to describe
// itself.
func (c *Catalog) scanSockets(ctx context.Context) ([]Available, error) {
	if c.SocketDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(c.SocketDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("plugins: socket directory: %w", err)
	}
	var out []Available
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		path := filepath.Join(c.SocketDir, e.Name())
		m, err := DescribeSocket(ctx, path)
		if err != nil {
			c.logger().Warn("plugin socket skipped", "socket", path, "error", err.Error())
			continue
		}
		if m.Name != strings.TrimSuffix(e.Name(), ".sock") {
			c.logger().Warn("plugin socket skipped", "socket", path, "error", "manifest name "+m.Name+" doesn't match the socket's")
			continue
		}
		out = append(out, Available{Manifest: *m, Source: registry.PluginSourceSocket, Path: path,
			Trust: registry.PluginTrustUnsigned, Isolation: IsolationContainer})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out, nil
}

// DialSocket connects to a plugin's unix socket.
func DialSocket(ctx context.Context, path string) (*rpc.Conn, error) {
	return dialSocket(ctx, path, nil)
}

func dialSocket(ctx context.Context, path string, onNotify func(string, json.RawMessage)) (*rpc.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	return rpc.NewConn(conn, conn, onNotify), nil
}

// DescribeSocket asks the plugin on path for its manifest, validated.
func DescribeSocket(ctx context.Context, path string) (*Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	conn, err := DialSocket(ctx, path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return describe(ctx, conn)
}

// describe runs the handshake's describe on conn and validates the
// answer.
func describe(ctx context.Context, conn *rpc.Conn) (*Manifest, error) {
	var m Manifest
	if err := conn.Call(ctx, protocol.MethodDescribe, nil, &m); err != nil {
		return nil, fmt.Errorf("plugins: describe: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
