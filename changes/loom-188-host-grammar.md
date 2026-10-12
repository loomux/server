### Changed

- A remote target's host may be a fully qualified name with its trailing dot (`box.example.net.`) or a link-local IPv6 address with its zone id (`fe80::1%eth0`), both of which ssh accepts as a destination (LOOM-188). A zone is an interface name or index; it is refused on any address that isn't link-local.

### Fixed

- The SSH ControlPath escapes `%`, which ssh would otherwise read as a token, so a config-mode target whose host carries a zone id connects.
