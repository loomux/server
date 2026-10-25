### Fixed

- A plugin upgrade whose new version passed its check but couldn't be stored (a database error writing its configuration or its row) no longer leaves the new version serving while the database still describes the old one (LOOM-245). The new version is stored before it takes over; if storing fails, it is stopped, the old version keeps running and its row is left as it was, and the upgrade answers `500`.
