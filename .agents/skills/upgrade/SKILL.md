---
name: "upgrade"
description: "update compatibility code when github.com/cbrgm/githubevents/v2 has a new release"
---

# upgrade

Update the local go-github compatibility shim when Dependabot bumps `github.com/cbrgm/githubevents/v2`.

## When to use

Use this skill when Dependabot opens a PR bumping `github.com/cbrgm/githubevents/v2`. `githubevents` re-exposes go-github's versioned types in its callback signatures, so a bump there can require a matching `github.com/google/go-github/vNN` major bump in this repo.

## Steps

1. Inspect the updated `github.com/cbrgm/githubevents/v2` module to find which `github.com/google/go-github/vNN/github` import path it now uses. Check both its `go.mod` and the callback signatures that this repo uses, especially `OnPushEventAny`.
2. Treat that discovered `vNN` as authoritative. The PR is incomplete until every repository reference to the versioned go-github module uses that same major version.
3. Ensure `go.mod` directly requires the authoritative `github.com/google/go-github/vNN` major version. If Dependabot left the new version as `// indirect` while the old version is still direct, promote the new version to direct and remove the old direct requirement.
4. Run `go mod tidy` so unused old `github.com/google/go-github/vMM` entries are removed from `go.mod` and `go.sum`.
5. Update `internal/githubcompat/githubcompat.go` so its import path matches the authoritative `vNN`. This is mandatory, not optional; a dependency-only diff is wrong if this file still imports the previous go-github major version.
6. Keep `githubcompat.PushEvent` defined as a type alias (`type PushEvent = github.PushEvent`) so it stays identity-equivalent to the upstream type and works as a `*github.PushEvent` callback parameter without conversion.
7. Do not change `main.go` unless the githubevents callback API itself changed. `main.go` should only reference `*githubcompat.PushEvent`.
8. Verify there is exactly one versioned go-github major referenced by repo code and module files:
   - `grep -R "github.com/google/go-github" -n . --exclude-dir=.git`
   - `go build ./...`
   - `go vet ./...`
   - `gofmt -l .`

## Required outcome

- `go.mod` has the same direct `github.com/google/go-github/vNN` requirement used by the updated `githubevents` module.
- `internal/githubcompat/githubcompat.go` imports `github.com/google/go-github/vNN/github` with that same `vNN`.
- No obsolete versioned go-github major remains in `go.mod`, `go.sum`, or source imports.
