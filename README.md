![ekilied — Ekilie Cloud Platform Agent](./docs/banner.png)

**`ekilied`** is the Ekilie Cloud platform agent daemon: a single lightweight Go binary that runs on your VPS and connects outbound to the Ekilie Cloud control plane. It manages sites, deployments, SSL certificates, Docker containers, system services, and its own updates.

- Outbound connections only. No inbound ports are needed beyond SSH.
- Runs as root, because it manages apt packages, nginx, supervisor, systemd units, site directories, and root's SSH keys.
- Linux is the production platform. The agent relies on `apt-get`, `systemctl`, `nginx`, `supervisor`, and `/root/.ssh`. Cross-builds for macOS and Windows exist for development only, and self-update assets are published for Linux only.

## How it works

```
┌──────────────┐     WSS (job triggers, heartbeats)     ┌──────────────────┐
│              │ ◄──────────────────────────────────── │                  │
│  Ekilie      │     HTTP (register, claim, logs,       │   ekilied agent  │
│  Cloud       │           complete, poll)              │   (on your VPS)  │
│  Control     │ ─────────────────────────────────────► │                  │
│  Plane       │                                        │   ┌──────────┐  │
│              │                                        │   │ JobEngine │  │
└──────────────┘                                        │   │ - sites   │  │
                                                        │   │ - nginx   │  │
                                                        │   │ - deploy  │  │
                                                        │   │ - env     │  │
                                                        │   │ - node/bun│  │
                                                        │   │ - ssl     │  │
                                                        │   │ - ssh     │  │
                                                        │   │ - daemon  │  │
                                                        │   │ - diag    │  │
                                                        │   │ - update  │  │
                                                        │   └──────────┘  │
                                                        └──────────────────┘
```

### Job lifecycle

Jobs arrive over two redundant channels and are always claimed over HTTP. Every connection is initiated by the agent, so the control plane never dials into your server:

| Channel | Purpose |
|---|---|
| WebSocket (primary) | Real-time triggers pushed over the agent's outbound connection: `job` (job ID only) and `job_full` (action and params inline) |
| HTTP poll (fallback) | The agent polls `GET /agents/jobs` over its outbound connection on the configured interval, catching jobs missed while the WebSocket was down |

1. A trigger arrives over WebSocket or is found by the poll loop. In-memory dedup and the atomic claim endpoint make sure a job executes once even if both channels deliver it.
2. `POST /agents/jobs/:id/claim` claims the job and returns its action and params. A job already claimed elsewhere is rejected with `409`.
3. The action runs, and log lines are buffered and sent in batches with `POST /agents/jobs/:id/logs`. Jobs that produce nothing for 30 seconds emit a `[heartbeat] still running` log line.
4. `POST /agents/jobs/:id/complete` reports `success` or `failed` with an optional result payload. If the agent dies first, the control plane redelivers the job.

WebSocket connections are kept alive with a 30 second ping, and reconnect automatically after a drop.

### Other agent traffic

| Traffic | Endpoint / message | Notes |
|---|---|---|
| Registration | `POST /agents/register` | One-time, authenticated with the instance registration token |
| Heartbeat | WS `heartbeat`, fallback `POST /agents/heartbeat` | Every 30 seconds by default: CPU, memory, disk, load average, host uptime (plus agent process uptime), hostname, platform, kernel arch, agent version |
| Capabilities | Sent during registration | Probes for `nginx`, `node`, `npm`, `docker`, `certbot`, `git`, `systemctl`, `php`, `composer` |
| Docker | WS `list_containers`, `log_stream`, `log_stream_stop` | Only when the Docker socket is reachable; streams container logs with a bounded tail, up to 5 concurrent streams per connection |
| Token rotation | WS `token_rotated` | Updates the in-memory session token |

### Session lifecycle

- `ekilied --setup` registers once and writes `agent_id` and `session_token` into `/etc/ekilie/agent.yml`.
- Starting the daemon with a registration token instead of `--setup` performs in-process registration and writes the session back to the config file atomically with mode `0600`.
- On startup, when the config has no session token but the local SQLite database has a saved identity, the agent resumes that identity instead of attempting a re-registration. Registration tokens are single use on the control plane, so this prevents a stranded agent after a restart.

## Actions

| Action | Description |
|---|---|
| `site_create` | Create `/opt/ekilie/sites/<name>/current`, write `.env` from params, and install an HTTP-only nginx vhost that proxies the domain to `127.0.0.1:<port>` (default 3000) with an ACME challenge passthrough. Idempotent. |
| `site_delete` | Remove all supervisor programs for the site, then delete `/opt/ekilie/sites/<name>` recursively. |
| `site_sync` | Clone or fetch and checkout the site repository into `current/` without writing `.env` or running a deploy script. Shares the per-site deploy lock with `deploy`. |
| `command` | Run an arbitrary bash command inside `/opt/ekilie/sites/<name>/current`, creating the directory and an empty `.env` if needed. Output streams to job logs. |
| `install_nginx` | Install nginx with `apt-get`. |
| `install_node` | Add the NodeSource 22.x repository, install `nodejs` and `npm`, then install `pm2` globally. |
| `install_bun` | Install prerequisites, run the official Bun installer, and symlink `bun` into `/usr/local/bin`. |
| `deploy` | Acquire the per-site deploy lock, clone or pull the repository (shallow, optional token and commit SHA), copy `env.example` to `.env` when no `.env` exists, write `.env` from params, write `.ekilie-deploy`, and run it from the repo root with output streamed to job logs. |
| `read_env` | Read the site `.env` and return its content as the job result. |
| `write_env` | Write raw `.env` content supplied in params. |
| `update_env` | Write `.env` from a key/value map supplied in params. |
| `site_raw_nginx` | Write a raw nginx config to `/etc/nginx/sites-available/<name>`, symlink it into `sites-enabled`, validate with `nginx -t`, then enable and reload nginx. |
| `read_nginx_config` | Read and return the site's nginx config as the job result. |
| `ssl_issue` | Issue a certificate with certbot in nginx mode (`--non-interactive --agree-tos --redirect`). |
| `ssh_key_add` | Append a public key to `/root/.ssh/authorized_keys` (duplicate-safe, mode `0600`). |
| `ssh_key_remove` | Remove a public key from `/root/.ssh/authorized_keys`. |
| `service_restart` | Restart an allowlisted systemd service: `nginx`, `supervisor`, or a per-site `ekilie-<site>` unit. Anything else fails the job. |
| `daemon_install_supervisor` | Install supervisor with apt, then enable and start the service. |
| `daemon_create` | Write a supervisor program named `<site>-<name>` that runs as the `ekilie` user with `numprocs=<scale>` in the site's `current/` directory, logs under `sites/<name>/logs`, then reload supervisor. |
| `daemon_delete` | Stop, remove, and delete a supervisor program config. |
| `daemon_restart` | Restart a supervisor-managed program. |
| `diagnostics` | Collect CPU, memory, disk, load, uptime, OS, and network I/O details, then return a structured result. Useful as a health and connectivity probe. |
| `self_update` | Check GitHub for a newer release, download the platform tarball, verify the SHA256 checksum, smoke-test the new binary, swap it in (keeping a `.bak`), and restart the agent. Concurrent updates are rejected with an explicit "already in progress" error. |

## Self-update

- Enabled by default (`auto_update: true`).
- Checks for a new release immediately on startup, then every `update_check_interval` (default 86400 seconds).
- Can also be triggered manually with `ekilied --update` or by a control-plane `self_update` job.
- Only one update runs at a time. The startup check, the 24 hour ticker, the manual flag, and a control-plane job cannot interleave the binary swap.
- Release assets are the Linux tarballs `ekilied-linux-amd64.tar.gz` and `ekilied-linux-arm64.tar.gz` plus `checksums.txt`, produced by `make release`.
- On a systemd host the restart is queued with `systemctl --no-block restart ekilied` so the job result is reported before the process is stopped. Outside systemd the agent re-executes the new binary in place with `syscall.Exec`.
- A failed restart is reported as a failed job instead of a silent success; the old binary keeps running and the next check retries.
- Checksums are verified, but release artifacts are not cryptographically signed yet.

## Configuration

Configuration is layered, and later sources override earlier ones:

1. Built-in defaults
2. YAML config file, `/etc/ekilie/agent.yml` (override the path with `--config` or `-c`)
3. CLI flags
4. Environment variables

Example `agent.yml`:

```yaml
server_id: 42
agent_id: agt_42
session_token: ek_session_xxx
api_url: https://engine.ekilie.cloud
ws_url: wss://engine.ekilie.cloud/api/v1/agents/ws
poll_interval: 5
heartbeat_interval: 30
auto_update: true
update_check_interval: 86400
```

`agent_id` and `session_token` are written back automatically after registration. The write is atomic, and the file is forced to mode `0600` because it holds credentials.

### Flags

| Flag | Shorthand | Description |
|---|---|---|
| `--config` | `-c` | Config file path (default `/etc/ekilie/agent.yml`) |
| `--api-url` | `-a` | Control plane URL, for example `https://engine.ekilie.cloud` |
| `--ws-url` | | WebSocket URL. Defaults to `wss://<api host>/api/v1/agents/ws`, derived from `--api-url`; the control plane can override it at registration |
| `--token` | `-t` | Registration token, or a session token when it starts with `ek_session_` |
| `--server-id` | `-s` | Server / instance ID |
| `--db-path` | | SQLite database path (default `<data-dir>/agent.db`) |
| `--data-dir` | | Data directory (default `/opt/ekilie/agent`) |
| `--log-level` | | `debug`, `info`, `warn`, or `error` |
| `--poll-interval` | | Job poll interval in seconds |
| `--heartbeat-interval` | | Heartbeat interval in seconds |
| `--auto-update` | | Enable automatic self-update. Only overrides `auto_update` from the config file when explicitly passed |
| `--update-interval` | | Update check interval in seconds (default `86400`) |
| `--setup` | | Run the one-time registration and write the config, then exit |
| `--update` | | Check for an update, replace the binary, and exit |
| `--version` | `-v` | Print version and exit |
| `--help` | `-h` | Print usage and exit |

### Environment variables

`EKILIED_API_URL`, `EKILIED_WS_URL`, `EKILIED_SERVER_ID`, `EKILIED_SESSION_TOKEN`, `EKILIED_REGISTRATION_TOKEN`, `EKILIED_DB_PATH`, `EKILIED_DATA_DIR`, `EKILIED_LOG_DIR`, `EKILIED_SOCKET_PATH`, `EKILIED_LOG_LEVEL`, `EKILIED_POLL_INTERVAL`, `EKILIED_HEARTBEAT_INTERVAL`, `EKILIED_AUTO_UPDATE`, `EKILIED_UPDATE_INTERVAL`.

### Key defaults

| Setting | Default |
|---|---|
| Config file | `/etc/ekilie/agent.yml` |
| Data directory | `/opt/ekilie/agent` |
| Database path | `/opt/ekilie/agent/agent.db` |
| Log directory | `/var/log/ekilie` |
| Socket path | `/var/run/ekilie/agent.sock` |
| Log level | `info` |
| Poll interval | 5 seconds, used whether or not the WebSocket is connected |
| Heartbeat interval | 30 seconds |
| Auto-update | Enabled, checked at startup and every 24 hours |

Notes:

- The registration response can override `poll_interval` and `ws_url`. Precedence: server-provided value > environment > flags > config file > defaults. The effective poll interval is logged at startup.
- Boolean flags are tri-state: `--auto-update` only overrides the config file when passed explicitly, so `auto_update: false` in `agent.yml` is honored. The effective value and its source are logged at startup.
- `log_dir`, `socket_path`, and `log_level` are parsed and stored but not yet used by the daemon. Treat them as reserved.
- When `--data-dir` is set, the database defaults to `<data-dir>/ekilied.db` unless `--db-path` is given.

## Quick start

Install as root (the script installs the binary, writes `/etc/ekilie/agent.yml`, registers the agent, and installs the `ekilied.service` systemd unit):

```bash
curl -fsSL https://ekilie.cloud/ekilied/install.sh | sudo bash -s -- \
  --token=<registration-token> \
  --server-id=<instance-id> \
  --api-url=https://engine.ekilie.cloud
```

Or register manually, then start the daemon:

```bash
sudo ekilied --setup \
  --api-url https://engine.ekilie.cloud \
  --server-id 42 \
  --token your-registration-token

sudo systemctl start ekilied
```

Installed locations:

| Path | Contents |
|---|---|
| `/usr/local/bin/ekilied` | Binary |
| `/etc/ekilie/agent.yml` | Config and credentials (mode `0600`) |
| `/etc/ekilie/agent.env` | Optional systemd environment file |
| `/etc/systemd/system/ekilied.service` | Unit with `Restart=always`, `RestartSec=10` |

## Build and test

Requires Go 1.25 or newer. Builds use `CGO_ENABLED=0` (pure Go SQLite), `-trimpath`, and `-s -w`, with version and commit injected via `-ldflags`.

| Command | Description |
|---|---|
| `make build` | Build `build/ekilied` for the host |
| `make dev` | Run with `go run ./cmd/ekilied` |
| `make test` | `go test -race -count=1 ./...` |
| `make lint` | `go vet ./...` |
| `make build-all` | Cross-build Linux, macOS, and Windows |
| `make build-linux` | Cross-build Linux amd64 and arm64, packaged as tarballs |
| `make release` | Tag and publish a GitHub release with Linux amd64 and arm64 tarballs plus `checksums.txt` |
| `make release-all` | Same as `release`, but publishes all platforms |
| `make checksums` | Regenerate SHA256 checksums in `build/` |

Custom version and commit:

```bash
go build -ldflags "-X github.com/ekilie/ekilied/internals/config.Version=1.2.3 \
  -X github.com/ekilie/ekilied/internals/config.Commit=$(git rev-parse --short HEAD)" \
  ./cmd/ekilied
```

### Tests

`make test` runs the suite with the race detector. Coverage currently includes the LogBatcher (batching, sequence, final flush), the restart paths (systemd and re-exec), the self-update concurrency guard, the startup update check, config parsing and session persistence, and session restore from the local database.

## Project structure

```
apps/ekilied/
├── cmd/ekilied/
│   ├── main.go               CLI entry: flags, setup, --update, registration, session restore, lifecycle
│   └── main_test.go          Session restore tests
├── internals/
│   ├── agent/
│   │   ├── agent.go          Lifecycle, capability detection, heartbeat/poll/update loops
│   │   ├── websocket.go      WSClient: connect/reconnect, message dispatch, HTTP helpers
│   │   ├── heartbeat.go      CPU, memory, disk, load, and host metric collection
│   │   ├── docker.go         Container listing and log streaming over the Docker socket
│   │   └── update_test.go    Startup update check tests
│   ├── jobengine/
│   │   ├── jobengine.go      JobEngine, DeployLock, LogBatcher, action dispatch
│   │   ├── restart.go        RestartAgent (systemd or re-exec) and update completion
│   │   ├── reexec_unix.go    syscall.Exec re-exec
│   │   ├── reexec_windows.go Manual restart error on Windows
│   │   ├── reexec_other.go   Manual restart error on other platforms
│   │   ├── self_update.go    GitHub release check, download, checksum, binary swap
│   │   ├── helpers.go        run(), splitLines(), formatting helpers
│   │   ├── sites.go          Site create, delete, sync, and command runner
│   │   ├── nginx.go          nginx install, config write/read, certbot
│   │   ├── node.go           Node.js and pm2 installer
│   │   ├── bun.go            Bun installer
│   │   ├── deploy.go         Repository clone/pull and env bootstrapping
│   │   ├── env.go            .env path resolution, read and write
│   │   ├── ssh.go            authorized_keys add and remove
│   │   ├── supervisor.go     Supervisor program CRUD and cleanup
│   │   ├── system.go         systemd service restart
│   │   └── diagnostics.go    Server diagnostics action
│   ├── config/config.go      Layered config, YAML read/write, SaveSession
│   ├── dtos/agent.go         API DTOs
│   └── models/models.go      GORM models for the local SQLite database
├── pkg/database/sqlite.go    Pure Go SQLite connection and migrations
├── Makefile
└── LICENSE
```

## Documentation

- [Codebase walkthrough](./docs/walkthrough.md): how ekilied works, layer by layer, with diagrams, a goroutine map, the full job lifecycle, and a suggested reading order.

## License

AGPL-3.0. See [LICENSE](./LICENSE).
