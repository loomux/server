# router

Router-model integration. See design spec §6.

A single swappable configuration setting, not hardcoded to a vendor.
Responsible for routing (workspace select / new-workspace provisioning /
direct answer, against compact registry metadata — not full workspace
history) and relay (condensing captured agent output into chat-appropriate
replies, and updating each workspace's rolling summary).
