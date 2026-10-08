### Security

- A cloned repository's output can no longer choose where its agent starts (LOOM-153). Provisioning used the first `loomux-workspace-path:` line on the screen, and a git server's messages print before the recipe's own report. The report now carries a random per-run tag, only the last tagged line counts, and the path is checked again to be inside the workspace root.
