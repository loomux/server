### Added

- A target's `plugin` object carries `progress` (LOOM-239): `image_pull` while a machine coming up pulls its image, empty otherwise. A client no longer has to read "pulling the image" from `status_reason`. This is an additive field in the API v1 contract.
