package kubernetes_test

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// kindContext is the context kind writes for the cluster
// deploy/test/kind/cluster/kind.yaml names.
const kindContext = "kind-loomux"

// kindKubectl is the kubeconfig and context TestKind gives every kubectl
// it runs, from env (KUBECONFIG's value): never the ambient default,
// which on a workstation can be a real cluster. KUBECONFIG must be one
// file whose current context is kind's, on loopback (where kind
// publishes its API server), and the cluster the plugin's kubeconfig
// names.
func kindKubectl(env string, pluginKubeconfig []byte) ([]string, error) {
	if env == "" || strings.ContainsRune(env, os.PathListSeparator) {
		return nil, fmt.Errorf("KUBECONFIG is %q: set it to the kind cluster's kubeconfig alone (kind create cluster --kubeconfig FILE), so kubectl never falls back to another cluster", env)
	}
	admin, err := clientcmd.LoadFromFile(env)
	if err != nil {
		return nil, fmt.Errorf("KUBECONFIG: %w", err)
	}
	plugin, err := clientcmd.Load(pluginKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("LOOMUX_KIND_KUBECONFIG: %w", err)
	}
	if admin.CurrentContext != kindContext {
		return nil, fmt.Errorf("KUBECONFIG's current context is %q, not kind's %q", admin.CurrentContext, kindContext)
	}
	server := serverOf(admin, kindContext)
	if u, err := url.Parse(server); err != nil || u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, fmt.Errorf("KUBECONFIG's %s is at %q, not on loopback as kind's is", kindContext, server)
	}
	if want := serverOf(plugin, plugin.CurrentContext); server != want {
		return nil, fmt.Errorf("KUBECONFIG's %s is at %q, the plugin's kubeconfig at %q", kindContext, server, want)
	}
	return []string{"--kubeconfig", env, "--context", kindContext}, nil
}

func serverOf(c *clientcmdapi.Config, context string) string {
	if ctx := c.Contexts[context]; ctx != nil {
		if cl := c.Clusters[ctx.Cluster]; cl != nil {
			return cl.Server
		}
	}
	return ""
}

// kubeconfigYAML is a kubeconfig with one context per cluster server,
// named as given; current names the current one.
func kubeconfigYAML(current string, servers map[string]string) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\nclusters:\n")
	for name, server := range servers {
		b.WriteString("  - name: " + name + "\n    cluster:\n      server: " + server + "\n")
	}
	b.WriteString("users:\n  - name: u\n    user:\n      token: t\ncontexts:\n")
	for name := range servers {
		b.WriteString("  - name: " + name + "\n    context:\n      cluster: " + name + "\n      user: u\n")
	}
	b.WriteString("current-context: " + current + "\n")
	return b.String()
}

// TestKindKubectlRefusesOtherClusters: TestKind's kubectl runs only
// against the kind cluster the plugin's kubeconfig names, with that
// kubeconfig and context spelled out, never the ambient default.
func TestKindKubectlRefusesOtherClusters(t *testing.T) {
	const kind = "https://127.0.0.1:41234"
	plugin := []byte(kubeconfigYAML("plugin", map[string]string{"plugin": kind}))
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("kind", kubeconfigYAML("kind-loomux", map[string]string{"kind-loomux": kind}))
	for _, tc := range []struct {
		name, env string
		plugin    []byte
	}{
		{"unset", "", plugin},
		{"a list, which could hold the default file", good + string(os.PathListSeparator) + filepath.Join(dir, "other"), plugin},
		{"missing", filepath.Join(dir, "nope"), plugin},
		{"another context current", write("current", kubeconfigYAML("admin@real", map[string]string{"admin@real": "https://10.0.0.5:6443", "kind-loomux": kind})), plugin},
		{"no kind context", write("nokind", kubeconfigYAML("kind-loomux", map[string]string{"admin@real": "https://10.0.0.5:6443"})), plugin},
		{"another kind cluster than the plugin's", write("otherkind", kubeconfigYAML("kind-loomux", map[string]string{"kind-loomux": "https://127.0.0.1:50000"})), plugin},
		{"not on loopback", write("remote", kubeconfigYAML("kind-loomux", map[string]string{"kind-loomux": "https://10.0.0.5:6443"})), []byte(kubeconfigYAML("plugin", map[string]string{"plugin": "https://10.0.0.5:6443"}))},
		{"plugin kubeconfig unreadable", good, []byte("{")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := kindKubectl(tc.env, tc.plugin)
			if err == nil {
				t.Fatalf("kindKubectl(%q) = %q, want an error", tc.env, args)
			}
			t.Log(err)
		})
	}
	args, err := kindKubectl(good, plugin)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--kubeconfig", good, "--context", "kind-loomux"}; !reflect.DeepEqual(args, want) {
		t.Errorf("kindKubectl = %q, want %q", args, want)
	}
}
