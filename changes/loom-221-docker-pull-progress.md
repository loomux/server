### Fixed

- A slow first image pull on the Docker plugin is no longer a create failure (LOOM-221, as LOOM-203 for Kubernetes). While a create, a recreate or a start remaking a lost container pulls the agent image, the plugin reports `progress: image_pull` alongside "pulling the image …". The host's 10-minute deadline stands still meanwhile, for up to 30 minutes more. Any other step reports no progress, so a machine stuck elsewhere still fails at 10 minutes.
