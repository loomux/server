### Fixed

- A remote target's `host` is checked against a host name grammar on create and update (LOOM-175): a host name, an ssh_config `Host` alias (letters, digits, `-` and `_` in dot-separated labels) or an IPv4/IPv6 address, else `400`. Before, anything without a leading `-`, whitespace, `@`, `/` or `\` was stored, including `%` tokens and shell metacharacters ssh would only fail on later. Managed targets keep LOOM-138's stricter grammar (no `_`). An existing target whose host doesn't fit can't be updated until its host is corrected.
