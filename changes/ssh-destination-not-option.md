### Security

- A target's `user` or `host` can no longer be read by ssh as an option:
  a value like `-oProxyCommand=…` ran a command inside the Loomux server.
  Targets now refuse a user that isn't a login name and a host with a
  leading `-`, whitespace, `@` or `/`, and every ssh call puts `--` before
  `user@host`, which also covers targets stored before this check.
