# go_caddy_url_updater

Small Go service listening for GitHub push webhooks and updating a Caddy configuration before triggering a reload. On startup it also checks the configured branch on GitHub and, if the local Caddyfile hash lags behind, performs the update and reload automatically. Core logic resides in [main.go](main.go) via [`main.updateCaddyfile`](main.go) and [`main.reloadCaddyInContainer`](main.go).

## Prerequisites

- Go 1.25 or newer
- Docker socket access (default `$ /var/run/docker.sock $`)
- Running Caddy container

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `CADDYFILE_PATH` | `/etc/caddy/Caddyfile` | Path to the Caddyfile inside the container |
| `CADDY_CONTAINER` | `caddy` | Container name for reload execution |
| `DOCKER_SOCK` | `/var/run/docker.sock` | Docker socket path used for API calls |
| `GITHUB_SECRETKEY` | `secret` | Secret for webhook signature verification |
| `DISCORD_WEBHOOK_URL` | _(empty)_ | Discord webhook endpoint for success and failure notifications |
| `LOCATION` | _(empty)_ | Optional server location label included in Discord messages |
| `GITHUB_REPO_OWNER` | `intro-skipper` | Owner used when checking the deployed binary against GitHub |
| `GITHUB_REPO_NAME` | `manifest` | Repository name used for commit verification |
| `GITHUB_REPO_BRANCH` | `main` | Branch considered the canonical reference for commit verification |
| `GITHUB_TOKEN` | _(empty)_ | Optional token for GitHub commit lookups (avoids low rate limits) |

## Run

```sh
go run ./...
```

The HTTP server exposes `/hook` on port 8080 and expects GitHub `push` events.

## License

Apache-2.0. This matches the strongest license requirement among the direct dependencies while remaining compatible with the BSD-3-Clause transitive dependencies.
