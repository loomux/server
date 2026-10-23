### Fixed

- A dispatch to a machine whose image is still pulling no longer fails after 5 minutes (LOOM-222). The 5-minute wait for a machine coming up stands still while the plugin reports the pull in progress (`progress: image_pull`), for up to 30 minutes more, as the machine's own create deadline does since LOOM-203. A machine stuck any other way still fails the dispatch at 5 minutes. A pull that never ends fails it once the 30 minutes are used too, and the error says so.
