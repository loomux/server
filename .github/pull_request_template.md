## What and why

<!-- What this changes, and the problem it solves. Link the issue: "Fixes #123". -->

## How it was tested

<!-- The tests you added or ran, and anything you checked by hand. -->

## Checklist

- [ ] `gofmt -l .` is empty, and `go vet ./...` and `go test -race ./...` pass (inside `plugins/<name>` too if a plugin changed)
- [ ] A fix comes with a test that fails without it
- [ ] A change users or operators would notice has a `changes/<slug>.md` fragment (`changes/README.md`)
- [ ] A change to `/api/v1` is recorded in the contract (`go test ./api -run TestAPIv1Contract -update`), and anything removed or changed was agreed in an issue first
- [ ] Docs are updated where an operator would look (`docs/deploy/`, `api/README.md`)

<!-- No CLA. A DCO sign-off (git commit -s) is welcome but optional. See CONTRIBUTING.md. -->
