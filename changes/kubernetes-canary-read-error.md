### Fixed

- The Kubernetes plugin's network-isolation check reports a read error that lasted the canary's whole time (etcd "leader changed", say) even when the canary's first read was also its last, because making the pod took most of that time. Before, it reported only "the canary didn't finish".
