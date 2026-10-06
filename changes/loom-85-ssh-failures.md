### Changed

- A target that can't be reached over SSH now says why (LOOM-85): host
  key changed or unknown, key refused, DNS, SOCKS proxy down, connection
  refused, host unreachable, shared-connection session refused, or
  timeout, each with a hint ("the host key of jet01 has changed: …").
  The class is in the error text and in
  `loomux_target_op_errors_total{reason="unreachable_<class>"}`, and
  ssh's banner lines (the post-quantum warning) no longer replace the
  real error or leak into command output.
