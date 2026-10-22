package kubernetes

import (
	"context"
	"sort"
	"strings"
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
// Otherwise it is starting, and its node's canary starts when a get,
// list or health first sees the pod on its node (the renewer knows only
// pods seen on a node). A node found not enforcing makes each "none"
// machine on it an error (its pod is left as it is, like any machine in
// error: destroy or recreate it) and withdraws "none" from describe and
// create; check reports it, and tries the node again with a canary
// pinned there a minute on, until one whose probe began after that
// finding's ended says "enforced" (a node's verdicts go by when their
// probes ran: nodeCheck). A canary that couldn't run there (no room, the image) proves nothing either way: an
// "enforced" from within verdictTTL still stands, and the node is tried
// again a minute on.
//
// A renewer keeps the nodes of "none" machines current: every
// RenewEvery it starts a canary on each such node whose "enforced" is
// older than half of verdictTTL, whether or not the host asks about the
// machines, so a machine in use stays running, and one that gets out
// turns them to error. What is known is in this process only: a
// restarted plugin checks each node again, its machines starting until
// then, and so does a configure: a request that took the configuration
// before it neither reads nor records the new one's verdicts, nor
// starts its canaries (stale). Each canary is a pod in the namespace's
// quota while it runs: up to one per node being checked, plus the
// check's own, for each configuration (a restart, a rollout, a
// configure) whose canaries are still live; one lives at most
// CanaryTimeout plus a minute (its active deadline, should this process
// die first). One that never got a node has no deadline (the kubelet
// keeps it); the renewer's sweep, at configure and once a minute, takes
// those a dead plugin left past that age. Too little room costs only
// time: a canary that can't be made proves nothing.

// nodeCheck is one node's canaries: the one running there; the last
// finished and the last that said "enforced", each the latest to start
// (seq); and the last that got out (open), with when the escapes since
// the node opened had all finished probing (openUntil, by the kubelet;
// zero if any time is missing).
//
// Verdicts go by when the probes ran, not when their canaries were
// started or read: a canary may wait on its image pull and run after
// one started later. A canary that got out is evidence of an escape
// during its probe. It opens the node unless the latest "enforced"
// probe began after it had ended (one that overlapped it may have missed
// what it saw); without both kubelet times it opens it.
// An open node clears only for an "enforced" probe known to have begun
// after the escapes' probes ended: by the kubelet's times, or because
// its canary started after the last escape was read. One that ran
// alongside an escape proves no recovery, and one that couldn't run
// proves nothing.
type nodeCheck struct {
	running, settled, enforced, open *netcheck
	openUntil                        time.Time
}

// record is a finished canary's verdict on the node; p.mu is held.
func (n *nodeCheck) record(nc *netcheck) {
	newer := func(old *netcheck) bool { return old == nil || nc.seq > old.seq }
	if newer(n.settled) {
		n.settled = nc
	}
	switch nc.state {
	case enforcementEnforced:
		if newer(n.enforced) {
			n.enforced = nc
		}
		if n.open != nil && n.clearedBy(nc) {
			n.open, n.openUntil = nil, time.Time{}
		}
	case enforcementNotEnforced:
		if n.enforced != nil && nc.probe.known() && n.enforced.probe.known() && nc.probe.until.Before(n.enforced.probe.from) {
			break // probed wholly before the latest "enforced" did: older news
		}
		switch {
		case n.open == nil:
			n.openUntil = nc.probe.until
		case !n.openUntil.IsZero() && nc.probe.known():
			if nc.probe.until.After(n.openUntil) {
				n.openUntil = nc.probe.until
			}
		default:
			n.openUntil = time.Time{}
		}
		if n.open == nil || nc.doneSeq > n.open.doneSeq {
			n.open = nc
		}
	}
}

// clearedBy is whether an "enforced" canary's probe surely began after
// the open node's escapes ended; p.mu is held.
func (n *nodeCheck) clearedBy(nc *netcheck) bool {
	if nc.seq > n.open.doneSeq {
		return true // started after the last escape was read
	}
	return !n.openUntil.IsZero() && nc.probe.known() && nc.probe.from.After(n.openUntil)
}

// due is whether the node's canary should start now: none is running,
// the node isn't open (that stands until a check), the last couldn't
// run more than a minute ago if it couldn't, and it has no "enforced"
// from the last half of verdictTTL. p.mu is held.
func (p *Plugin) due(n *nodeCheck, now time.Time) bool {
	switch last := n.settled; {
	case n.running != nil || n.open != nil:
		return false
	case last == nil:
		return true
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
	p.seq++
	nc := &netcheck{done: make(chan struct{}), state: enforcementPending, node: node, seq: p.seq}
	p.nodeOf(node).running = nc
	go p.runCanary(nc, cfg, client, instance)
	return nc
}

// reconfigured is the verdict's detail for a request that took an
// earlier configuration than the plugin's now.
const reconfigured = "the plugin was reconfigured meanwhile; ask again"

// stale is whether cfg, as a request took it, is an earlier
// configuration than the plugin's now: such a request neither reads
// nor writes this one's verdicts, nor starts its canaries. p.mu is held.
func (p *Plugin) stale(cfg Config) bool { return cfg.gen != p.gen }

// renewNodes is the renewer of one configuration, until stop. It also
// sweeps the canaries left behind, at once and then once a minute: one
// that never got a node isn't ended by its deadline (the kubelet's), and
// a plugin that died before deleting it would otherwise leave it in the
// quota until some later canary's own sweep.
func (p *Plugin) renewNodes(stop <-chan struct{}, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var swept time.Time
	for {
		if now := p.Now(); swept.IsZero() || now.Sub(swept) >= time.Minute {
			p.sweepLeft()
			swept = now
		}
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

// sweepLeft is sweepCanaries for the configuration now.
func (p *Plugin) sweepLeft() {
	p.mu.Lock()
	cfg, client, instance := p.cfg, p.client, p.host.InstanceID
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p.sweepCanaries(ctx, client.CoreV1().Pods(cfg.Namespace), instance)
}

// unwatch forgets a machine's pod, gone or going: its node is no longer
// renewed for it. One of an earlier configuration forgets nothing.
func (p *Plugin) unwatch(cfg Config, pod string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stale(cfg) {
		delete(p.watch, pod)
	}
}

// retestNodes starts a canary on each open node last tried more than a
// minute ago, and returns those running on open nodes; p.mu is held.
func (p *Plugin) retestNodes(cfg Config, client kubernetes.Interface, instance string) []*netcheck {
	var running []*netcheck
	for node, n := range p.nodes {
		if n.open == nil {
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

// nodeFailure is "not enforced" while some node is open, naming each
// (by name) and what its canary reached; "enforced" otherwise.
func (p *Plugin) nodeFailure() (enforcementState, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var bad []string
	for node, n := range p.nodes {
		if n.open != nil {
			bad = append(bad, node)
		}
	}
	if len(bad) == 0 {
		return enforcementEnforced, ""
	}
	sort.Strings(bad)
	var detail []string
	for _, node := range bad {
		detail = append(detail, "on node "+node+", "+p.nodes[node].open.detail)
	}
	return enforcementNotEnforced, strings.Join(detail, "; ")
}

// nodeOpen is "not enforced" while node is open, naming it and what
// its canary reached; pending for an earlier configuration; "enforced"
// otherwise.
func (p *Plugin) nodeOpen(cfg Config, node string) (enforcementState, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stale(cfg) {
		return enforcementPending, reconfigured
	}
	if n := p.nodes[node]; node != "" && n != nil && n.open != nil {
		return enforcementNotEnforced, "on node " + node + ", " + n.open.detail
	}
	return enforcementEnforced, ""
}

// knownLeak is "not enforced" while the latest finished canary, or one
// on some node, found a confined pod getting out; "enforced" otherwise:
// the last word before a "none" pod is made (ensurePod); pending for an
// earlier configuration.
func (p *Plugin) knownLeak(cfg Config) (enforcementState, string) {
	p.mu.Lock()
	last, stale := p.settled, p.stale(cfg)
	p.mu.Unlock()
	if stale {
		return enforcementPending, reconfigured
	}
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
			p.unwatch(cfg, pod.Name)
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
	if p.stale(cfg) {
		if status == protocol.EnvRunning {
			return protocol.EnvStarting, "the plugin was reconfigured meanwhile; checking network isolation on node " + node + " again"
		}
		return status, reason
	}
	p.watch[pod.Name] = node
	n, now := p.nodeOf(node), p.Now()
	if p.due(n, now) {
		p.startNodeCanary(cfg, client, instance, node)
	}
	switch last := n.settled; {
	case n.open != nil:
		return protocol.EnvError, "egress none isn't enforced on node " + node + " (" + n.open.detail + "): this machine isn't confined; destroy it, and fix the policy engine there before making another"
	case status != protocol.EnvRunning:
		return status, reason
	case n.enforced != nil && now.Sub(n.enforced.at) <= verdictTTL:
		return status, reason
	case last != nil && last.state != enforcementEnforced:
		return protocol.EnvStarting, "network isolation on node " + node + " couldn't be checked (" + last.detail + "); trying again"
	}
	return protocol.EnvStarting, "checking network isolation on node " + node
}
