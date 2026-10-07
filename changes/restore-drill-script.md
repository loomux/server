### Added

- `deploy/restore-drill.sh` runs the restore drill (LOOM-127) end to
  end: the newest backup through a read-only pod, the image on a private
  copy with throwaway credentials, and the checks (integrity, migration,
  health, login, what's served), cleaning everything up and printing a
  drill-record row.
