### Security

- The SSH ControlMaster socket directory (`${TMPDIR}/loomux/ssh-cm`) and
  its parent are checked before every ssh run (LOOM-166): a symlink, a
  non-directory or one owned by another user is refused rather than
  used, and both are made 0700, so another local user can't plant a
  socket ssh would take for the master.
