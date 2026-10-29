### Fixed

- An agent is sent the task, not the routing instructions around it: the
  parts of a message that only say which agent, workspace or machine to
  use ("Use Claude Code in workspace api-server. The agent should …") are
  left out of what the agent gets, so it no longer answers about Loomux
  itself (LOOM-258). The conversation keeps the message as typed; text is
  only ever taken out, and a message that is nothing but routing goes
  whole.
- The routing model no longer asks for confirmation itself: a direct
  answer asking the user to confirm or reply "yes" (copying Loomux's own
  offer text) has that request taken out and says nothing is waiting for
  a yes, since no offer was recorded for it to approve (LOOM-255). A
  question asking for information ("Could you confirm which workspace you
  mean?") stays.
