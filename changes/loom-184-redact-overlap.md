### Fixed

- Redaction of vault and system secrets replaces the union of every match (LOOM-184): two values that partly overlap in the text (one's tail the other's head), or a value repeated so that its matches overlap, no longer leave part of one behind.
