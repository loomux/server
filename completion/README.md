# completion

Tiered completion detection. See design spec §5.

Three tiers, strongest first: (1) native agent-CLI hooks wired to a
side-channel, (2) prompt-engineered self-report via a file/socket marker
for tool-use-capable agents without native hooks, (3) idle-time heuristic
as a last resort. Agents run in native interactive mode throughout — this
was a deliberate design correction from an earlier headless-mode direction
that would have blocked live SSH interaction with a running agent.
