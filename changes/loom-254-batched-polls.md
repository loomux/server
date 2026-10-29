### Changed

- Turns waiting on the same remote target share its completion polls
  (LOOM-254): one ssh operation a second per target checks every waiting
  turn's marker file and pane, where each turn used to make two of its
  own. Ten sessions on one host behind a ~300 ms link no longer fill the
  target's 6-operation budget, which had queued every other operation
  (the router's `has_session`, `capture_pane`) behind the polls: in a
  loopback load test, target-op p95 at 10 sessions went from 1.55 s to
  0.31 s. A pane the shared poll doesn't see running is still asked about
  on its own, so exit statuses and errors are reported as before.
- The no-progress check of a marker-tier turn captures its pane less
  often while the pane stays unchanged: every 5 s, backing off to 15 s,
  and back to 5 s on any change. A prompt is still confirmed 5 s after
  it is seen.

### Added

- `loomux_target_op_seconds` has a 2 s bucket, the p95 bar, and a new
  `op="poll"` series for the shared polls.
