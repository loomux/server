### Fixed

- The Kubernetes plugin's check no longer reports OK when reading the headless Service fails for a reason other than not found or forbidden, such as a timeout or a server error (LOOM-246). It now warns `service_missing` with the API server's message, since whether machine names resolve is unknown.
