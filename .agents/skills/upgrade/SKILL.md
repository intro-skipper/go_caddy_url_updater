---
name: "upgrade"
description: "update compatibility code when github.com/cbrgm/githubevents/v2 has a new release"
---

# upgrade

Update the local go-github compatibility shim when Dependabot bumps `github.com/cbrgm/githubevents/v2`.

## When to use

Use this skill when Dependabot opens a PR bumping `github.com/cbrgm/githubevents/v2`. `githubevents` re-exposes go-github's versioned types in its callback signatures, so a bump there can require a matching `github.com/google/go-github/vNN` major bump in this repo.

## Steps

1. Inspect the updated `github.com/cbrgm/githubevents/v2` module to find which `github.com/google/go-github/vNN/github` import path it now uses (check its `go.mod` and the callback signatures, e.g. `OnPushEventAny`).
2. Ensure `go.mod` requires that same `github.com/google/go-github/vNN` major version. Run `go mod tidy` if needed.
3. Update `internal/githubcompat/githubcompat.go` so its import path matches the same `vNN` (this is the only file in the repo that should reference the versioned go-github import).
4. Keep `githubcompat.PushEvent` defined as a type alias (`type PushEvent = github.PushEvent`) so it stays identity-equivalent to the upstream type and works as a `*github.PushEvent` callback parameter without conversion.
5. Do not change `main.go` unless the githubevents callback API itself changed. `main.go` should only reference `*githubcompat.PushEvent`.
6. Verify with `go build ./...`, `go vet ./...`, and `gofmt -l .`.
