package kubernetes

import (
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Loomux/server/plugins/protocol"
)

// The per-node check (LOOM-208). The canary of netcheck.go runs on
// whichever node the scheduler picks, and NetworkPolicy is enforced by
// an engine on each node: one missing from or broken on another node
// leaves the machines there unconfined. So an egress "none" machine is
// reported running only once a canary bound to its own node
// (spec.nodeName, past the scheduler) has said "enforced" there, at
// most verdictTTL ago; until then it is starting, and its pod is
// checked while it comes up. A node found not enforcing makes each
// "none" machine on it an error (its pod is left as it is, like any
// machine in error: destroy or recreate it) and withdraws "none" from
// describe and create; check reports it, and tries the node again with
// a canary pinned there a minute on, until one says "enforced". A
// canary that couldn't run there (no room, the image) proves nothing
// either way: the machine stays starting and the node is tried again a
// minute on.
//
// A machine once let run stays running while its node's verdict ages:
// past half of verdictTTL, when a machine there is asked about, a
// canary renews it in the background, and only a "not enforced" turns
// the machine to error. What is remembered is in this process only: a
// restarted plugin checks each node again, its machines starting until
// then. The canary needs room for one more pod in the namespace's
// quota.

// nodeCheck is one node's canaries: the latest started there, still
// running, and the latest finished.
type nodeCheck struct{ running, settled *netcheck }

// nodeOf is node's entry; p.mu is held.
func (p *Plugin) nodeOf(node string) *nodeCheck {
	n := p.nodes[node]
	if n == nil {
		n = &nodeCheck{}
		p.nodes[node] = n
	}
	return n
}

// startNodeCanary starts a canary pinned to node; p.mu is held.
func (p *Plugin) startNodeCanary(cfg Config, client kubernetes.Interface, instance, node string) *netcheck {
	nc := &netcheck{done: make(chan struct{}), state: enforcementPending, node: node}
	p.nodeOf(node).running = nc
	go p.runCanary(nc, cfg, client, p.apiHost, instance)
	return nc
}

// retestNodes starts a canary on each node found not enforcing more
// than a minute ago, and returns those running on such nodes; p.mu is
// held.
func (p *Plugin) retestNodes(cfg Config, client kubernetes.Interface, instance string) []*netcheck {
	var running []*netcheck
	for node, n := range p.nodes {
		if n.settled == nil || n.settled.state != enforcementNotEnforced {
			continue
		}
		if n.running == nil && p.Now().Sub(n.settled.at) > time.Minute {
			p.startNodeCanary(cfg, client, instance, node)
		}
		if n.running != nil {
			running = append(running, n.running)
		}
	}
	return running
}

// nodeFailure is "not enforced" while a canary's last word on some node
// is that, naming the node (the first by name); "enforced" otherwise.
func (p *Plugin) nodeFailure() (enforcementState, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var bad []string
	for node, n := range p.nodes {
		if n.settled != nil && n.settled.state == enforcementNotEnforced {
			bad = append(bad, node)
		}
	}
	if len(bad) == 0 {
		return enforcementEnforced, ""
	}
	sort.Strings(bad)
	return enforcementNotEnforced, "on node " + bad[0] + ", " + p.nodes[bad[0]].settled.detail
}

// nodeGate is an egress "none" machine's status given its pod's: what
// the pod says, unless its node isn't known to confine it. Asking
// starts the node's canary when it has no verdict, when "enforced" is
// older than half of verdictTTL, or when a canary couldn't run there a
// minute ago.
func (p *Plugin) nodeGate(cfg Config, client kubernetes.Interface, instance string, pod *corev1.Pod, status, reason string) (string, string) {
	if status == protocol.EnvError || status == protocol.EnvStopped {
		return status, reason
	}
	node := pod.Spec.NodeName
	if node == "" {
		if status == protocol.EnvRunning {
			return protocol.EnvStarting, "waiting to learn the pod's node, to check network isolation there"
		}
		return status, reason
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n, now := p.nodeOf(node), p.Now()
	last := n.settled
	if n.running == nil && (last == nil ||
		last.state == enforcementEnforced && now.Sub(last.at) > verdictTTL/2 ||
		last.state != enforcementEnforced && last.state != enforcementNotEnforced && now.Sub(last.at) > time.Minute) {
		p.startNodeCanary(cfg, client, instance, node)
	}
	confirmation := string(pod.UID) + "@" + node
	switch {
	case last != nil && last.state == enforcementNotEnforced:
		delete(p.confirmed, pod.Name)
		return protocol.EnvError, "egress none isn't enforced on node " + node + " (" + last.detail + "): this machine isn't confined; destroy it, and fix the policy engine there before making another"
	case status != protocol.EnvRunning:
		return status, reason
	case p.confirmed[pod.Name] == confirmation:
		return status, reason
	case last != nil && last.state == enforcementEnforced && now.Sub(last.at) <= verdictTTL:
		p.confirmed[pod.Name] = confirmation
		return status, reason
	case last != nil && last.state != enforcementEnforced:
		return protocol.EnvStarting, "network isolation on node " + node + " couldn't be checked (" + last.detail + "); trying again"
	}
	return protocol.EnvStarting, "checking network isolation on node " + node
}
