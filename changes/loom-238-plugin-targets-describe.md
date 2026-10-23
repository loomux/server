### Added

- `GET /api/v1/plugins/{id}/targets` returns what a plugin instance offers for a new machine, as its `targets.describe` says right now (LOOM-238): its sizes, whether machines keep their data by default, its egress options, its image, its machine quota and how many machines it has. The web's create-machine form no longer has to guess these from the instance's configuration. The response is `404` for no such instance and `409` when the instance isn't running or doesn't make machines. This is an additive route in the API v1 contract.
