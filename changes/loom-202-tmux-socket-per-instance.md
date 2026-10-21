### Fixed

- The tmux socket (`LOOMUX_TMUX_SOCKET`) is each server instance's own, never a process-wide setting the last start overwrote (LOOM-202). Two instances in one process used to run every session on the socket the second one named, where its orphan sweep would reap the first's sessions as its own; each one's sessions, sweep, environment scrub and attach commands now use its own socket.
