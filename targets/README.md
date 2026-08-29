# targets

Execution targets — where a workspace's tmux session actually runs. See
design spec §1.

Defines the `TargetExecutor` interface (`RunTmuxCommand`, `CapturePane`,
`KillSession`, etc.) with a plain-SSH v1 implementation for remote targets;
`local` targets shell out directly. Designed so a future companion-daemon
implementation is a second implementation of the same interface, not a
redesign.
