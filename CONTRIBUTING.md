# Contributing to Loomux

Thanks for looking. Loomux is pre-alpha and maintained by one person, so
reviews and answers are best effort. This page says what helps most
right now and how to get a change merged.

## What's welcome before 1.0

- **Bug reports** with what you ran, what happened and how to make it
  happen again. Use the bug template.
- **Fixes** for bugs, with a test that fails without the fix.
- **Docs** that were wrong, unclear or missing when you set Loomux up.
- **Support for another agent CLI version**: a prompt or screen Loomux
  doesn't recognise yet, with the captured pane as a test.

Please **open an issue before you write a feature** or anything that
changes `/api/v1`, the database schema, or a decision recorded in
[`docs/design/core-design.md`](docs/design/core-design.md). The design
was argued through on purpose (for example, agents run in their native
interactive mode, never headless), and a short issue saves you a pull
request that can't be merged. Loomux is single-user by design for now.

The web client is [loomux/web](https://github.com/loomux/web) and the
terminal client is [loomux/tui](https://github.com/loomux/tui); changes
to them go there.

## Security problems

Don't report a vulnerability in a public issue, pull request or
discussion. Follow [`SECURITY.md`](SECURITY.md).

## Building and testing

You need the Go version in [`go.mod`](go.mod) and tmux. The SQLite
driver is pure Go, so there's no cgo toolchain to install.

What CI runs (`.github/workflows/ci.yml`), from the repository root:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test -race -shuffle=on ./...
sh deploy/changelog.sh check
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

The plugins under `plugins/kubernetes` and `plugins/docker` are their
own Go modules: run `gofmt -l .`, `go vet ./...` and
`go test -race ./...` inside each one you change.

Tips for a local run:

- Tests start tmux servers on sockets of their own and remove them
  afterwards. A socket's path must fit the 108-byte limit for Unix
  sockets, so if `TMUX_TMPDIR` (or `/tmp`) is a long path, point
  `TMUX_TMPDIR` at a short directory such as `/tmp/lx`.
- The `targets` tests start an SSH server of their own
  (`targets/sshtest`). If your ssh-agent holds several keys, they can
  fail with "too many authentication failures"; run them with
  `env -u SSH_AUTH_SOCK`.
- A new agent screen (a prompt Loomux doesn't recognise yet) is tested
  from a captured pane in `agents/testdata`.

## Making a change

- **One change per pull request**, small enough to review in one
  sitting. Say what it fixes or adds and how you tested it.
- **Tests:** a fix comes with a test that fails without it; a feature
  comes with tests for what it promises.
- **Changelog:** a change a user or operator would notice adds a
  fragment, `changes/<slug>.md`, instead of editing `CHANGELOG.md`.
  The format is in [`changes/README.md`](changes/README.md).
- **API:** `TestAPIv1Contract` records every `/api/v1` route, field,
  status code, query parameter and header. An addition is recorded with
  `go test ./api -run TestAPIv1Contract -update`; removing or changing
  something there is a v1 break and needs an issue first. See
  [`docs/release/versioning.md`](docs/release/versioning.md), "The API
  v1 contract".
- **Docs:** a new setting, route or behaviour is described where an
  operator would look for it (`docs/deploy/`, `api/README.md`).
- **Commits:** a short summary line saying what changed, then why in the
  body. Maintainers' commits carry a `LOOM-<n>` ticket number; yours
  don't need one.

Every merge to `main` is released as a patch version, so `main` must
stay green.

## Licence, CLA and DCO

Loomux is under the [MIT licence](LICENSE), and your contribution is
accepted under the same licence. There is **no CLA** to sign. A
[Developer Certificate of Origin](https://developercertificate.org/)
sign-off (`git commit -s`) is welcome but not required.
