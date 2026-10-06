# Changelog fragments

A pull request with a user-visible change adds its `CHANGELOG.md` entry
here as its own file instead of editing `CHANGELOG.md`, so open pull
requests never conflict over the same `[Unreleased]` lines. A milestone
(each MINOR, an rc, 1.0.0) folds them into `CHANGELOG.md` with
`deploy/changelog.sh release VERSION DATE`; see
`docs/release/versioning.md`.

One file per pull request, named for it (`loom-85-ssh-failures.md`).
Inside, Keep a Changelog sections (`### Added`, `### Changed`,
`### Deprecated`, `### Removed`, `### Fixed`, `### Security`), each with
`- ` entries:

```markdown
### Changed

- A target that can't be reached over SSH now says why (LOOM-85): …

### Fixed

- …
```

CI runs `deploy/changelog.sh check`; `deploy/changelog.sh assemble`
prints what the next milestone's section will be.
