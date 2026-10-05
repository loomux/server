### Added

- An agent that keeps compacting its context within one turn (3 times
  in 30 minutes) has the turn failed as `compaction_loop` and is
  interrupted, its pane kept, instead of running to the turn's time
  bound (LOOM-109; Claude Code).
