package kubernetes

import (
	"net"
	"net/url"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ClientFactory makes the clientset for a configuration; tests swap in
// a fake. It also returns the API server's host:port, which the
// enforcement canary probes. Its errors never repeat the kubeconfig.
type ClientFactory func(cfg Config) (kubernetes.Interface, string, error)

// defaultClient is in-cluster (the mounted ServiceAccount: the sidecar
// form) unless a kubeconfig is configured (a subprocess outside the
// cluster, tests).
func defaultClient(cfg Config) (kubernetes.Interface, string, error) {
	var rc *rest.Config
	var err error
	if cfg.Kubeconfig != "" {
		rc, err = clientcmd.RESTConfigFromKubeConfig([]byte(cfg.Kubeconfig))
		if err != nil {
			return nil, "", invalid("kubeconfig: not a kubeconfig the client accepts")
		}
	} else {
		rc, err = rest.InClusterConfig()
		if err != nil {
			return nil, "", invalid("no kubeconfig configured and not in a cluster: %v", err)
		}
	}
	rc.UserAgent = "loomux-plugin-kubernetes/" + Version
	rc.Timeout = 30 * time.Second
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, "", invalid("kubeconfig: the client refuses it")
	}
	return cs, apiHostPort(rc.Host), nil
}

// apiHostPort is host:port of the API server from a rest.Config Host.
func apiHostPort(host string) string {
	u, err := url.Parse(host)
	if err != nil || u.Host == "" {
		return ""
	}
	h, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		h, p = u.Host, "443"
		if u.Scheme == "http" {
			p = "80"
		}
	}
	return net.JoinHostPort(h, p)
}
