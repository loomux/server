### Fixed

- A plugin's `max_environments` can no longer be exceeded by two machine creates at once (LOOM-244). Before, two requests, or two server processes, could each count one place left and both create a machine. The environment row is now counted and inserted in one statement, so exactly one gets the last place. The other is refused with `409` before any machine, target or key is made.
