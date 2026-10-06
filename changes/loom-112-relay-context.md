### Changed

- The relay model is told what a turn was for (LOOM-112): the user's
  message, the workspace's summary from before the turn, and the agent
  type, with the captured output last. A terse answer is relayed as the
  answer to its question, and the reply says so when the output doesn't
  show the asked-for work. Message and summary are scrubbed like the
  output and bounded (2000 and 1500 characters).
