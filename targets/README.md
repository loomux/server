# targets

Execution targets — where a workspace's tmux session actually runs. See
design spec §1.

Defines the `TargetExecutor` interface (`RunTmuxCommand`, `CapturePane`,
`KillSession`, etc.) with a plain-SSH v1 implementation for remote targets;
`local` targets shell out directly. Designed so a future companion-daemon
implementation is a second implementation of the same interface, not a
redesign.

## Layout

- `targets.go` — the `TargetExecutor` interface, `ErrUnreachable`, and
  `NewExecutor(t *registry.Target)`, which branches on `t.Kind` so
  callers never need to know whether they're talking to a local or
  remote executor.
- `local.go` — `LocalExecutor`: shells out straight to the `tmux` binary
  for tmux operations, no SSH involved; `FileExists`/`RemoveFile` are
  plain `os.Stat`/`os.Remove`; `RunOnce` shells out via `sh -c` directly
  (not `tmux` — a genuine one-shot command, not a pane operation).
- `remote.go` — `RemoteExecutor`: shells out to the real `ssh` binary
  per target operation, with SSH connection multiplexing
  (`ControlMaster`/`ControlPersist`) so repeated calls against the same
  target reuse one connection. Real targets are expected to resolve via
  the caller's own `~/.ssh/config` (same convention as `ssh wyzer`,
  `ssh jet01` elsewhere in this ecosystem); `RemoteOption`s
  (`WithPort`, `WithIdentityFile`, `WithExtraSSHArgs`,
  `WithConnectTimeout`) exist for tests to point at a non-standard
  target, not for production use. `FileExists`/`RemoveFile` run
  `test -e`/`rm -f` remotely (existence only — content was never the
  signal); all SSH invocations (tmux subcommands and these two, plus
  `RunOnce`) share one `sshExec` helper so the connection-reuse/quoting/
  error-handling conventions can't drift between them.
- `RunOnce` (both executors, design spec §10 axis 3, LOOM-17) runs a
  command once, non-interactively, and returns its combined stdout+stderr
  — distinct from the session methods above (`NewSession`/`SendKeys`/
  `CapturePane`, all oriented around a long-running interactive tmux
  pane). Its first real consumer is `router.VersionCheck` (a launch-time
  `claude --version`-style check), but it's a general-purpose primitive,
  not version-check-specific.
- `executortest/` — shared behavioral test suite (mirrors
  `registry/storetest`'s pattern): `executortest.Run` runs identical
  assertions (including `RunOnce`) against any `TargetExecutor`, proving
  `local` and `remote` are actually interchangeable behind the interface.
- `sshtest/` — test-only in-process SSH server used by `remote_test.go`
  to exercise the real `ssh` client against a loopback target, per the
  design spec's Testing Strategy section — no system `sshd` or dedicated
  system user required.

Run `go test ./...` from the repo root to run the full suite, including
the SSH-backed tests.
