package router

import "strings"

// MaxOfferedWorkspaces bounds how many workspaces one routing call sees
// (LOOM-107): every one adds a prompt entry and a schema enum value, so
// an unbounded list grows the prompt, its cost and the model's chance
// to pick wrong without limit.
const MaxOfferedWorkspaces = 25

// capOffered keeps at most max of offered (already most recently used
// first): always the ones the message is most likely about, then the most
// recent. Always kept: the client's workspace hint, the conversation's
// last workspace and its open task's, and any workspace the message
// names. The result keeps offered's order.
func capOffered(offered []WorkspaceSnapshot, max int, message string, mustKeep ...string) []WorkspaceSnapshot {
	if len(offered) <= max {
		return offered
	}
	offeredIDs := make(map[string]bool, len(offered))
	for _, ws := range offered {
		offeredIDs[ws.ID] = true
	}
	keep := make(map[string]bool, max)
	// Only IDs that are offered: a hint naming an archived or deleted
	// workspace mustn't take a slot (#221 review).
	for _, id := range mustKeep {
		if offeredIDs[id] {
			keep[id] = true
		}
	}
	lower := strings.ToLower(message)
	for _, ws := range offered {
		if mentions(lower, ws.Name) {
			keep[ws.ID] = true
		}
	}
	// Recency fills what's left; pinned ones beyond max still stay.
	for _, ws := range offered {
		if len(keep) >= max {
			break
		}
		keep[ws.ID] = true
	}
	out := make([]WorkspaceSnapshot, 0, len(keep))
	for _, ws := range offered {
		if keep[ws.ID] {
			out = append(out, ws)
		}
	}
	return out
}

// mentions reports whether message (lowercased) names the workspace:
// its name as written, or with dashes and underscores read as spaces.
// Names under 3 characters are too likely to match by chance.
func mentions(message, name string) bool {
	name = strings.ToLower(name)
	if len([]rune(name)) < 3 {
		return false
	}
	if strings.Contains(message, name) {
		return true
	}
	spaced := strings.NewReplacer("-", " ", "_", " ").Replace(name)
	return spaced != name && strings.Contains(message, spaced)
}
