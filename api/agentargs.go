package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/Loomux/server/registry"
)

// agentArgsJSON is one agent type's pinned launch args (LOOM-307) as the
// API spells them: both fields always present, "" for not pinned. A
// request naming any other field is refused, so a client can't believe
// it passed a flag Loomux dropped.
type agentArgsJSON struct {
	// Model is the model the agent type is launched with on this target.
	Model string `json:"model"`
	// Effort is its effort (reasoning) level, one its CLI takes.
	Effort string `json:"effort"`
}

func (a *agentArgsJSON) UnmarshalJSON(b []byte) error {
	type plain agentArgsJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("agent_args: each entry is {\"model\": ..., \"effort\": ...}: %w", err)
	}
	*a = agentArgsJSON(p)
	return nil
}

func newAgentArgsJSON(a registry.AgentArgs) agentArgsJSON {
	return agentArgsJSON{Model: a.Model, Effort: a.Effort}
}

// agentArgsResponse is a target's pins keyed by agent type; {} for none,
// never null.
func agentArgsResponse(args map[string]registry.AgentArgs) map[string]agentArgsJSON {
	out := make(map[string]agentArgsJSON, len(args))
	for name, a := range args {
		out[name] = newAgentArgsJSON(a)
	}
	return out
}

// agentArgsFromRequest is a request's agent_args as the registry stores
// them: an entry pinning nothing is left out, and none at all is nil.
func agentArgsFromRequest(req map[string]agentArgsJSON) map[string]registry.AgentArgs {
	var out map[string]registry.AgentArgs
	for name, a := range req {
		pin := registry.AgentArgs{Model: a.Model, Effort: a.Effort}
		if pin.IsZero() {
			continue
		}
		if out == nil {
			out = map[string]registry.AgentArgs{}
		}
		out[name] = pin
	}
	return out
}

// AgentArgsCheck reports whether agentType is one this server launches
// and its CLI takes the pin a (router.AgentTypeRegistry.CheckAgentArgs).
type AgentArgsCheck func(agentType string, a registry.AgentArgs) error

// WithAgentArgsCheck checks each target's agent_args against the agent
// types this server launches (LOOM-307). Without it only their form is
// checked; the launch still refuses a pin its CLI can't take.
func WithAgentArgsCheck(check AgentArgsCheck) Option {
	return func(s *Server) { s.agentArgsCheck = check }
}

// checkAgentArgs validates target's agent_args, form first (the
// registry's rules, also for a plugin machine, whose target skips
// Validate here), then against the agent types. It writes the 400 itself
// and reports whether the caller should continue.
func (s *Server) checkAgentArgs(w http.ResponseWriter, target *registry.Target) bool {
	if err := registry.ValidateAgentArgs(target.AgentArgs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if s.agentArgsCheck == nil {
		return true
	}
	names := make([]string, 0, len(target.AgentArgs))
	for name := range target.AgentArgs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.agentArgsCheck(name, target.AgentArgs[name]); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("agent_args[%q]: %v", name, err))
			return false
		}
	}
	return true
}
