### Security

- A host key scan can only be pinned for the address it was read from
  (LOOM-164): a scan still running when a `PUT` changed the target's
  host, port, proxy or key, or a pin racing such a `PUT`, is refused
  with `409` instead of pinning the old machine's key for the new
  address.
