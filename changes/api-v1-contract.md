### Added

- An API v1 contract test: `api/testdata/api-v1-contract.txt` records
  every `/api/v1` route and the JSON fields of its bodies, and
  `TestAPIv1Contract` fails when one is removed or changed (API v1's
  additions-only promise for 1.0.0) or added without being recorded
  (`go test ./api -run TestAPIv1Contract -update`). See
  `docs/release/versioning.md`.
