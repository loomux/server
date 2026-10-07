### Removed

- `ssh_key_ref` on targets (API v1 freeze review, item 1): it was stored
  and returned but nothing used it — SSH keys come from the mounted
  secret. Responses no longer carry it; a request that sends it has it
  ignored.
