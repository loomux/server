# registry

Workspace registry and pluggable storage layer. See design spec §2, §8.

Covers the `targets`/`workspaces`/`tasks` tables and the storage interface
that makes the underlying DB backend swappable, plus the migration story
for moving between backends.
