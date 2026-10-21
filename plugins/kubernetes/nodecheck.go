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
// reported running only while a canary bound to its own node
// (spec.nodeName, past the scheduler) has said "enforced" there within
// verdictTTL; the check's canary counts for the node it landed on.
// Otherwise it is starting, and its node's canary starts as soon as
// the pod has a node. A node found not enforcing makes each "none"
// machine on it an error (its pod is left as it is, like any machine in
// error: destroy or recreate it) and withdraws "none" from describe and
// create; check reports it, and tries the node again with a canary
// pinned there a minute on, until one says "enforced". A canary that
// couldn't run there (no room, the image) proves nothing either way: an
// "enforced" from within verdictTTL still stands, and the node is tried
// again a minute on.
//
// A renewer keeps the nodes of "none" machines current: every
// RenewEvery it starts a canary on each such node whose "enforced" is
// older than half of verdictTTL, whether or not the host asks about the
// machines, so a machine in use stays running, and one that gets out
// turns them to error. What is known is in this process only: a
// restarted plugin checks each node again, its machines starting until
// then. Each canary needs room for one more pod in the namespace's
// quota.

// nodeCheck is one node's canaries: the latest started there, still
// running; the latest finished; and the latest that said "enforced".
type nodeCheck struct{ running, settled, enforced *netcheck }

// record is a finished canary's verdict on the node; p.mu is held.
func (n *nodeCheck) record(nc *netcheck) {
	n.settled = nc
	if nc.state == enforcementEnforced {
		n.enforced = nc
	}
}

// due is whether the node's canary should start now: none is running,
// the last didn't find it open (that stands until a check), the last
// couldn't run more than a minute ago if it couldn't, and it has no
// "enforced" from the last half of verdictTTL. p.mu is held.
func (p *Plugin) due(n *nodeCheck, now time.Time) bool {
	switch last := n.settled; {
	case n.running != nil:
		return false
	case last == nil:
		return true
	case last.state == enforcementNotEnforced:
		return false
	case last.state != enforcementEnforced && now.Sub(last.at) <= time.Minute:
		return false
	}
	return n.enforced == nil || now.Sub(n.enforced.at) > verdictTTL/2
}

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

// renewNodes is the renewer of one configuration, until stop.
func (p *Plugin) renewNodes(stop <-chan struct{}, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		p.mu.Lock()
		if len(p.watch) == 0 {
			p.mu.Unlock()
			continue
		}
		now := p.Now()
		for _, node := range p.watch {
			if n := p.nodeOf(node); p.due(n, now) {
				p.startNodeCanary(p.cfg, p.client, p.host.InstanceID, node)
			}
		}
		p.mu.Unlock()
	}
}

// unwatch forgets a machine's pod, gone or going: its node is no longer
// renewed for it.
func (p *Plugin) unwatch(pod string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.watch, pod)
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

// knownLeak is "not enforced" while the latest finished canary, or one
// on some node, found a confined pod getting out; "enforced" otherwise:
// the last word before a "none" pod is made (ensurePod).
func (p *Plugin) knownLeak() (enforcementState, string) {
	p.mu.Lock()
	last := p.settled
	p.mu.Unlock()
	if last != nil && last.state == enforcementNotEnforced {
		return last.state, last.detail
	}
	return p.nodeFailure()
}

// nodeGate is an egress "none" machine's status given its pod's: what
// the pod says, unless its node isn't known to confine it. Asking
// starts the node's canary when it is due.
func (p *Plugin) nodeGate(cfg Config, client kubernetes.Interface, instance string, pod *corev1.Pod, status, reason string) (string, string) {
	if status == protocol.EnvError || status == protocol.EnvStopped {
		if status == protocol.EnvStopped {
			p.unwatch(pod.Name)
		}
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
	p.watch[pod.Name] = node
	n, now := p.nodeOf(node), p.Now()
	if p.due(n, now) {
		p.startNodeCanary(cfg, client, instance, node)
	}
	switch last := n.settled; {
	case last != nil && last.state == enforcementNotEnforced:
		return protocol.EnvError, "egress none isn't enforced on node " + node + " (" + last.detail + "): this machine isn't confined; destroy it, and fix the policy engine there before making another"
	case status != protocol.EnvRunning:
		return status, reason
	case n.enforced != nil && now.Sub(n.enforced.at) <= verdictTTL:
		return status, reason
	case last != nil && last.state != enforcementEnforced:
		return protocol.EnvStarting, "network isolation on node " + node + " couldn't be checked (" + last.detail + "); trying again"
	}
	return protocol.EnvStarting, "checking network isolation on node " + node
}
