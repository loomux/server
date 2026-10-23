### Fixed

- The Kubernetes plugin deletes each network-isolation canary pod once its verdict is recorded, so none is left `Completed` in the agents namespace (LOOM-212). A delete that fails is tried again by the once-a-minute sweep, whatever the pod's phase and even after a configure moved to another namespace, and it doesn't change the verdict. A canary still running for another check is left to it.
