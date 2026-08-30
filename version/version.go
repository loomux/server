// Package version holds loomuxd's own release version (design spec §10
// axis 4) — independent of the API version (axis 1, /api/v1/...): a
// server patch release doesn't force an API version bump, only a
// breaking API change does.
package version

// Version is loomuxd's semver release version. Overridable at build
// time via
//
//	go build -ldflags "-X github.com/Loomux/server/version.Version=1.2.3"
//
// "dev" when built without that flag (e.g. a local `go build`/`go run`).
var Version = "dev"
