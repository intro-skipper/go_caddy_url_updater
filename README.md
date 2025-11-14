# go_caddy_url_updater

Small Go service listening for GitHub push webhooks and updating a Caddy configuration before triggering a reload. Core logic resides in [main.go](main.go) via [`main.updateCaddyfile`](main.go) and [`main.reloadCaddyInContainer`](main.go).

## Prerequisites

- Go 1.25 or newer
- Docker socket access (default `$ /var/run/docker.sock $`)
- Running Caddy container

## Configuration

| Variable | Default | Purpose |
|----------|---------|---------|
| `CADDYFILE_PATH` | `/etc/caddy/Caddyfile` | Path to the Caddyfile inside the container |
| `CADDY_CONTAINER` | `caddy` | Container name for reload execution |
| `DOCKER_SOCK` | `/var/run/docker.sock` | Docker socket path used for API calls |
| `GITHUB_SECRETKEY` | `secret` | Secret for webhook signature verification |

## Run

```sh
go run ./...
```

The HTTP server exposes `/hook` on port 8080 and expects GitHub `push` events.
