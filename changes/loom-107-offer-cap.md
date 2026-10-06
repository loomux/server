### Changed

- The routing model sees at most 25 workspaces per message (LOOM-107):
  the most recently used, plus, however old, the one the client hinted,
  the conversation's last and open-task workspaces, and any the message
  names. Failed, archived and shell workspaces were already left out.
