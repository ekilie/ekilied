# ekilied

**`ekilied`** — Ekilie Cloud platform agent daemon. A lightweight Go binary that runs on your VPS and connects to the Ekilie Cloud control plane to manage sites, deployments, SSL certificates, system services, and more.

## How it works

```
┌──────────────┐     WSS (real-time triggers)     ┌──────────────────┐
│              │ ◄─────────────────────────────── │                  │
│  Ekilie      │     HTTP (job claim, logs,       │   ekilied agent  │
│  Cloud       │           heartbeat, poll)       │   (on your VPS)  │
│  Control     │ ───────────────────────────────► │                  │
│  Plane       │                                   │   ┌──────────┐  │
│              │                                   │   │ JobEngine │  │
└──────────────┘                                   │   │ - sites   │  │
                                                   │   │ - nginx   │  │
                                                   │   │ - node    │  │
                                                   │   │ - deploy  │  │
                                                   │   │ - ssl     │  │
                                                   │   │ - ssh     │  │
                                                   │   │ - systemd │  │
                                                   │   │ - daemon  │  │
                                                   │   │ - update  │  │
                                                   │   └──────────┘  │
                                                   └──────────────────┘
```

`ekilied` makes outbound connections only — no inbound ports needed beyond SSH.

### Dual-channel job reception

Jobs are received via two redundant channels:

| Channel | Direction | Purpose |
|---|---|---|
| **WebSocket** (primary) | Server → Agent | Real-time job triggers (`{"type":"job","payload":{"job_id":42}}`) |
| **HTTP poll** (fallback) | Agent → Server | Periodic `GET /agents/jobs` every 5s — catches jobs missed during WS reconnects |

When a trigger arrives (via either channel), the agent atomically claims the full job details via `POST /agents/jobs/:id/claim`, executes the action, streams logs, and reports completion — all over HTTP.

## Actions

| Action | Description |
|---|---|
| `site_create` | Create a site directory at `/opt/ekilie/sites/<name>` |
| `site_delete` | Remove a site directory and clean up supervisor configs |
| `install_nginx` | Install nginx via apt |
| `install_node` | Install Node.js 22.x + npm + pm2 via NodeSource |
| `deploy` | Clone/pull a git repo, copy `env.example` → `.env`, write control-plane env vars, run a deploy script |
| `update_env` | Write `.env` file for a site from control plane params |
| `site_raw_nginx` | Write a raw nginx config, validate with `nginx -t`, enable & reload |
| `ssl_issue` | Issue SSL certificate via certbot (nginx mode) |
| `ssh_key_add` | Add an SSH public key to `/root/.ssh/authorized_keys` (duplicate-safe) |
| `ssh_key_remove` | Remove an SSH public key from `authorized_keys` |
| `service_restart` | Restart a systemd service |
| `daemon_install_supervisor` | Install supervisor via apt and enable the service |
| `daemon_create` | Create a supervisor program config and reload |
| `daemon_delete` | Stop, remove, and delete a supervisor program config |
| `daemon_restart` | Restart a supervisor-managed program |
| `self_update` | Check GitHub for a newer release, download, verify checksum, swap binary, restart |

## Configuration

The agent uses a layered configuration system (later sources override earlier ones):

1. **Defaults** — hardcoded sensible defaults
2. **YAML config file** — `/etc/ekilie/agent.yml` (optional, overridable via `--config`)
3. **CLI flags** — `--api-url`, `--token`, `--server-id`, etc.
4. **Environment variables** — `EKILIED_API_URL`, `EKILIED_SESSION_TOKEN`, etc.

### Key defaults

| Setting | Default |
|---|---|
| Poll interval | 5s |
| Heartbeat interval | 30s |
| Data directory | `/opt/ekilie/agent` |
| Database path | `<data-dir>/agent.db` |
| Log directory | `/var/log/ekilie` |
| Socket path | `/var/run/ekilie/agent.sock` |
| Log level | `info` |
| Auto-update | enabled (checks every 24h) |

## Quick start

```bash
curl -fsSL https://ekilie.cloud/ekilied/install.sh | bash
```

Or register manually:

```bash
sudo ekilied --setup \
  --api-url https://engine.ekilie.cloud \
  --server-id 42 \
  --token your-registration-token
```

Then start the daemon:

```bash
sudo systemctl start ekilied
```

## Build

```bash
make build
sudo cp build/ekilied /usr/local/bin/ekilied
```

Build with custom version info:

```bash
go build -ldflags "-X github.com/ekilie/ekilied/internals/config.Version=1.2.3 \
  -X github.com/ekilie/ekilied/internals/config.Commit=$(git rev-parse --short HEAD)" \
  ./cmd/ekilied
```

## Project structure

```
internals/
├── agent/              Core agent: WebSocket client, heartbeat, Docker
│   ├── agent.go        Agent lifecycle, capability detection, loops
│   ├── websocket.go    WSClient — connect, message dispatch, HTTP helpers
│   ├── heartbeat.go    System metrics collection
│   └── docker.go       Docker container listing and log streaming
├── jobengine/          Job execution engine (standalone package)
│   ├── jobengine.go    Core: JobClient interface, DeployLock, LogBatcher, Execute switch
│   ├── helpers.go      run(), writeFile(), splitLines()
│   ├── sites.go        createSiteDir, removeSiteDir
│   ├── nginx.go        installNginx, writeNginxConfig, issueSSL
│   ├── node.go         installNode
│   ├── deploy.go       writeEnvFile, cloneRepo
│   ├── ssh.go          addSSHKey, removeSSHKey
│   ├── system.go       restartService
│   ├── supervisor.go   Supervisor program CRUD + cleanup
│   └── self_update.go  GitHub release check + binary self-update
├── config/config.go    Layered config: defaults → YAML → flags → env vars
├── dtos/agent.go       Shared DTOs for API communication
└── models/models.go    GORM models for local SQLite database
```

## License

AGPL v3
