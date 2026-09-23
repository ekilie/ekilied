# ekilied Codebase Walkthrough

A guided tour of how `ekilied` works, from process start to a finished deploy, with
diagrams for every moving part. It is written for a Go developer who has never seen
this repo and wants to understand it deeply, not just compile it.

Reading time is roughly 45 to 60 minutes. Keep the code open next to it: every section
names the files and functions it describes.

Companion material:

- [`README.md`](../README.md) for install, build, and configuration reference.
- `docs/cloud/ekilied/` in the parent repo for the control-plane contract
  (`api-contract.md`, `agent-auth-flow.md`, `vps-ekilied-architecture.md`).

---

## 1. What ekilied is

`ekilied` is a root daemon that runs on a VPS and connects outbound to the Ekilie Cloud
control plane. The control plane decides what should happen; `ekilied` makes it happen
on the machine and reports back.

```
        Ekilie Cloud control plane (Go API, Postgres)
        ┌───────────────────────────────────────────────┐
        │  users, billing, instances, job queue         │
        │  /agents/register  /agents/ws  /agents/jobs/* │
        └───────────────┬───────────────────────────────┘
                        │  all connections initiated by the agent
                        │  WSS for triggers + HTTP for work
                        ▼
        User's VPS (root)
        ┌───────────────────────────────────────────────┐
        │  ekilied (systemd service)                    │
        │   ├─ JobEngine    runs actions                │
        │   ├─ WSClient     talks to the control plane  │
        │   ├─ Docker       container list + log stream │
        │   └─ SQLite       local identity + cache      │
        │                                               │
        │  manages: nginx, sites, deploys, supervisor,  │
        │  certbot, systemd services, .env files        │
        └───────────────────────────────────────────────┘
```

Two properties define the whole design:

1. **Outbound only.** The agent dials out; nothing ever dials in. This is why no
   firewall changes are needed and why every loop has a reconnect story.
2. **The control plane is the source of truth.** Actions arrive as jobs. The agent
   claims a job before running it, streams logs while it runs, and reports the final
   status. If the agent restarts, the control plane redelivers.

---

## 2. The three layers

Everything in the repo fits into three layers. Learn them in this order.

```
┌──────────────────────────────────────────────────────────────────┐
│ TRANSPORT        internals/agent/websocket.go                    │
│                  one WebSocket + HTTP helpers                    │
│                  owns: connection, egress, dispatch              │
├──────────────────────────────────────────────────────────────────┤
│ SCHEDULING       internals/agent/agent.go                        │
│                  heartbeat, poll, update loops                   │
│                  owns: cadence, registration, capabilities       │
├──────────────────────────────────────────────────────────────────┤
│ EXECUTION        internals/jobengine/                            │
│                  JobEngine + action handlers                     │
│                  owns: claiming, running, logging, completing    │
└──────────────────────────────────────────────────────────────────┘
```

| Layer | Entry point | Main files |
|---|---|---|
| Transport | `WSClient` | `websocket.go`, `logstream.go` |
| Scheduling | `Ekilied` | `agent.go`, `heartbeat.go`, `docker.go` |
| Execution | `JobEngine` | `jobengine.go`, `validate.go`, plus one file per domain |

The dependency direction is deliberate: `jobengine` never imports `agent`. The
`JobClient` interface in `jobengine.go` is implemented by `agent.WSClient`, which
inverts the dependency and lets the engine be tested with a fake.

---

## 3. Startup: from `main()` to a running daemon

File: `cmd/ekilied/main.go`.

```
main()
 │
 ├─ parseFlags()                        flags + --help / --version short circuits
 │
 ├─ f.Setup?  ──► runSetup()            register once, write agent.yml, exit
 │
 ├─ f.Update? ──► CheckForUpdate()      download + swap binary, exit
 │                SelfUpdate()
 │
 ├─ config.Load(path, WithFlags(...))   defaults -> YAML -> flags -> env vars
 │
 ├─ database.Connect + AutoMigrate      local SQLite (identity, capabilities)
 │
 ├─ !cfg.HasSession()?
 │     └─ restoreSessionFromDB()        heal agents whose session was never saved
 │
 ├─ e := agent.New(cfg, db)             one agent for the whole boot
 │
 ├─ cfg.NeedsRegistration()?
 │     ├─ e.RegisterAndSave()           POST /agents/register + atomic identity write
 │     └─ config.SaveSession()          atomic 0600 write back to agent.yml
 │
 ├─ e.Start()                           capability scan + all long-lived loops
 │
 └─ <-SIGINT/SIGTERM ──► e.Stop()       cancel, close docker, wait, mark offline
```

### 3.1 Configuration layering

File: `internals/config/config.go`.

```
built-in defaults  ->  agent.yml  ->  CLI flags  ->  environment variables
     (weakest)                                            (strongest)
```

- `Defaults()` holds the values in the README defaults table.
- The YAML file is parsed with `gopkg.in/yaml.v3` into a pointer mirror struct,
  so a missing key is distinguishable from a zero value, unknown keys are rejected,
  and type errors fail startup with the offending line quoted.
- `WithFlags` applies flag overrides. A `--token` starting with `ek_session_` is
  treated as a session token, anything else as a registration token.
- `applyEnvOverrides` reads `EKILIED_*` variables last.
- `SaveSession(path, agentID, token)` writes credentials back to the YAML file with
  a temp file + `Sync` + rename, forcing mode `0600`.

When `ws_url` is empty it is derived from `api_url` by `deriveWsURL`: `https` becomes
`wss`, `http` becomes `ws`, and the path is completed to `/api/v1/agents/ws` without
duplicating an existing `/api/v1` or the full endpoint. The effective control plane
and WebSocket URLs are logged at startup (never the token).

The important behavior: after a successful registration the session token must be
persisted, because the registration token is single use on the control plane. If the
agent restarts without a session, it can never register again. That is why startup
has both a write-back (`SaveSession`) and a restore path (`restoreSessionFromDB`).

Boolean flags are tri-state. `--auto-update` only overrides the file when it is
explicitly passed (`--auto-update` or `--auto-update=false`), so `auto_update: false`
in `agent.yml` is honored. `optionalBool` in `cmd/ekilied/main.go` records whether the
flag was set, and the effective value plus its source (`default`, `config file`,
`flag`, `environment`) is logged at startup.

### 3.2 Local state

File: `internals/models/models.go`, `pkg/database/sqlite.go`.

| Model | Purpose |
|---|---|
| `Identity` | Last agent id, session, URLs, connection flag |
| `Capability` | Tool availability snapshot |

Only these two models are migrated. Job state lives in memory (`active` and
`dispatched` maps), and the old job, site, and setting tables were removed from
`AllModels`; they are empty leftovers in existing local databases.

The database is pure-Go SQLite (`glebarez/sqlite`) with a single connection. It is a
convenience cache, not the source of truth: losing it only costs one re-registration
at worst (and the session restore exists to avoid even that).

The identity row is replaced inside a single transaction (`saveIdentity` in
`internals/agent/agent.go`), so a crash or write error between the delete and the
insert cannot leave the agent with no identity. `RegisterAndSave` is safe to call
before `Start`, and `main` builds exactly one agent per boot.

---

## 4. The goroutine and context map

This is the part that trips up new readers, so learn it early. Every long-lived
goroutine and what stops it:

```
root context (Ekilied.ctx, cancelled by Stop)
│
├─ heartbeatLoop        ticker 30s          <- ctx.Done
├─ httpPollLoop         timer poll interval <- ctx.Done
├─ updateCheckLoop      ticker 24h          <- ctx.Done or update applied
│
└─ WSClient.Connect (loop forever)
   │
   └─ connectOnce (one per connection)
      │  connCtx = WithCancel(root)       <- cancelled when the connection ends
      │
      ├─ read pump       conn.Read(connCtx)   -> closes readCh on error
      ├─ egress pump     select connCtx/egress/egressLow
      ├─ ping pump       conn.Ping(connCtx) every 30s
      │
      └─ dispatch loop   for msg := range readCh
         │
         ├─ job / job_full    -> go onJob(rootCtx, ...)      survives reconnects
         ├─ list_containers   -> docker call on connCtx
         └─ log_stream        -> worker goroutine tracked in pumps, on connCtx
```

Rules that make this safe:

- **Per-connection pumps die with the connection.** `connectOnce` cancels `connCtx`
  and calls `pumps.Wait()` before returning, so a dead connection can never steal
  messages from the next one (this was a real bug, fixed in PR #56).
- **Job execution uses the root context**, so a job survives a WebSocket reconnect.
- **Streams and container listings use the connection context**, so they are cancelled
  when the connection drops.
- **Shutdown order:** `cancel()` -> docker close -> `wg.Wait()` -> mark offline.

---

## 5. Transport: the WebSocket client

File: `internals/agent/websocket.go`.

`WSClient` owns one WebSocket at a time plus two outbound queues:

| Field | Capacity | Purpose |
|---|---|---|
| `egress` | 32 | High priority: heartbeats, errors, control replies |
| `egressLow` | 128 | Low priority: container lists, log lines |
| `readCh` | 64 | Inbound messages waiting for the dispatch loop |

The egress pump drains high priority first, then low priority, so a flood of log
lines can never delay a heartbeat.

### 5.1 One connection, step by step

```
connectOnce(ctx)
 │
 ├─ connCtx = WithCancel(ctx); defer connCancel()
 ├─ pumps WaitGroup
 ├─ websocket.Dial(connCtx, wsUrl + "?token=" + session)
 ├─ setConn(conn); connected = true
 ├─ spawn read pump    -> readCh (drop when full, never block the socket)
 ├─ spawn egress pump  -> egress (high) then egressLow (low)
 ├─ spawn ping pump    -> 30s keepalive
 ├─ for msg := range readCh { dispatch }
 │
 └─ teardown: connCancel(); streams.stopAll(); pumps.Wait()
              connected = false; setConn(nil); return error
```

`Connect` is the outer loop: it calls `connectOnce` and retries forever with a fixed
5 second delay. If the agent is under systemd, the process itself is what restarts
after a self-update.

### 5.2 Message catalog

Inbound (control plane to agent), handled in the dispatch loop:

| Type | Payload | Effect |
|---|---|---|
| `job` | `{job_id}` | Claim and execute |
| `job_full` | `{job_id, action, params}` | Claim and execute without waiting for params |
| `token_rotated` | `{new_token}` | Update the in-memory session token |
| `job_cancelled` | | Logged; in-flight job is not interrupted |
| `list_containers` | | Reply with `container_list` |
| `log_stream` | `{container, tail, stream_id}` | Start an async Docker follow |
| `log_stream_stop` | `{stream_id}` | Cancel that follow |

Outbound (agent to control plane):

| Type | Payload | Sent by |
|---|---|---|
| `heartbeat` | metrics | `SendHeartbeat` (WS first, HTTP fallback) |
| `error` | `{error, message}` | `sendError` |
| `container_list` | containers | `list_containers` handler |
| `log_line` | one Docker line | log stream forwarder |

### 5.3 HTTP helpers

All job work happens over HTTP even when the trigger arrived over WebSocket:

| Method | Endpoint | Notes |
|---|---|---|
| `Register` | `POST /agents/register` | One time; returns session + `ws_url` |
| `SendHeartbeat` | `POST /agents/heartbeat` | Fallback when WS is down or full |
| `PollJobs` | `GET /agents/jobs` | Catches triggers missed while disconnected |
| `ClaimJob` | `POST /agents/jobs/:id/claim` | Atomic claim; HTTP 409 becomes `ErrJobAlreadyClaimed` |
| `StreamLogs` | `POST /agents/jobs/:id/logs` | Batched lines |
| `CompleteJob` | `POST /agents/jobs/:id/complete` | `success` or `failed` plus result |

### 5.4 Docker log streams

File: `internals/agent/logstream.go`.

A log stream is the one operation that could block forever, so it gets its own
machinery:

```
log_stream message
      │
      ├─ docker available?          no  -> error reply
      ├─ container/stream_id valid? no  -> error reply
      ├─ tail clamped to 100..2000
      ├─ registry.add(stream_id)    full or duplicate -> error reply
      │
      └─ goroutine (tracked in pumps)
            ├─ forwarder: logCh -> egressLow, lines truncated at 16KB
            ├─ docker.StreamLogs(streamCtx, follow=true)
            ├─ on return: close(logCh); forwarder.Wait()
            └─ deferred registry.remove(stream_id)
```

Limits: 5 concurrent streams, 2000 tail lines, 16KB per line. `log_stream_stop`
cancels by id; a connection drop cancels everything through `streams.stopAll()`.

---

## 6. The job lifecycle, end to end

Files: `internals/jobengine/jobengine.go`, `internals/agent/websocket.go`.

This is the core sequence. Read it once and most of the codebase becomes obvious.

```
Control plane                  Agent (WSClient)                Agent (JobEngine)
     │                               │                                │
     │  WS job_full {id,action,     │                                │
     │    params} ─────────────────►│                                │
     │                               │  go onJobFull(rootCtx,...)     │
     │                               │───────────────────────────────►│
     │                               │                                │ markDispatched(id)
     │                               │  POST /jobs/:id/claim          │
     │                               │◄───────────────────────────────│
     │  200 accepted + job ─────────►│                                │
     │                               │                                │ Execute(id,action,params)
     │                               │                                │ ├─ active guard
     │                               │                                │ ├─ semaphore (max 10)
     │                               │                                │ ├─ validateActionParams
     │                               │                                │ ├─ LogBatcher
     │                               │                                │ └─ action switch
     │                               │  POST /jobs/:id/logs (batches) │
     │                               │◄───────────────────────────────│
     │                               │  POST /jobs/:id/complete       │
     │                               │◄───────────────────────────────│
     │  200 ─────────────────────────►│                                │
```

### 6.1 Claim before execute

Both entry points claim first:

- `HandleJobTrigger` (WS `job` or HTTP poll): claim, then execute with the params
  from the claim response.
- `HandleJobTriggerFull` (WS `job_full`): claim, then execute with the params already
  received, skipping the response parse but still waiting for the 200.

Claim failures abort execution and clear the dispatch mark so the poll loop can
redeliver. A 409 (`ErrJobAlreadyClaimed`) means another worker owns the job and it is
never run here. This ordering is what guarantees the control plane never sees a
completion before the claim.

### 6.2 Deduplication and concurrency

Three guards work together:

| Guard | Type | Purpose |
|---|---|---|
| `dispatched` | `sync.Map` | One trigger per job id across WS and poll |
| `active` | `sync.Map` | One execution per job id even if triggers race |
| `semaphore` | `chan struct{}` cap 10 | Bounded concurrent jobs |
| `deployLk` | per-site mutex | One deploy per site at a time |

Cleanup runs in defers, in this order: log batcher close, semaphore release, active
and dispatched delete. A panic anywhere in an action is recovered, reported as a
failed job with the panic text, and the same defers keep the engine usable.

### 6.3 The LogBatcher

File: `internals/jobengine/jobengine.go` (top half).

```
Write / WriteErr / Writef / heartbeat
        │  appendLineLocked -> seq++  (monotonic across flushes)
        ▼
   lines []LogLine
        │  every 100ms: flushNow()
        ▼
   StreamLogs POST  (one request per non-empty batch)
```

Details that matter:

- `Sequence` is a cumulative counter on the batcher, so ordering survives flushes.
- The 30 second "still running" line is emitted only when there has been no output.
- `Close()` performs one final flush using `context.WithoutCancel` with a 10 second
  timeout, then cancels. This is why trailing lines are never lost.
- `Close` is idempotent (`sync.Once`) and safe to call from the job defer.

### 6.4 Error and output discipline

- `run()` streams stdout/stderr to the batcher and keeps only the last 8KB for the
  error message.
- The final `CompleteJob` error is truncated to 4KB, so a pathological command can
  never ship megabytes to the control plane.

---

## 7. Actions: the execution layer

The switch in `JobEngine.Execute` is the table of contents for the product. Each case
maps to a file.

| Action | File | What it does |
|---|---|---|
| `site_create` | `sites.go` | Dirs + `.env` + HTTP-only nginx vhost |
| `site_delete` | `sites.go` | Supervisor cleanup + recursive delete |
| `site_sync` | `sites.go` | Clone/fetch only, no deploy script |
| `command` | `sites.go` | Bash in `current/`, streams output |
| `deploy` | `jobengine.go`, `deploy.go` | Clone/pull, env bootstrap, run script |
| `read_env` / `write_env` / `update_env` | `env.go`, `deploy.go` | `.env` read/write |
| `site_raw_nginx` / `read_nginx_config` | `nginx.go` | Raw vhost management |
| `install_nginx` / `ssl_issue` | `nginx.go` | apt + certbot |
| `install_node` / `install_bun` | `node.go`, `bun.go` | Runtime installs |
| `ssh_key_add` / `ssh_key_remove` | `ssh.go` | `authorized_keys` |
| `service_restart` | `system.go` | `systemctl restart` |
| `daemon_*` | `supervisor.go` | Supervisor program CRUD |
| `diagnostics` | `diagnostics.go` | Metrics report |
| `self_update` | `self_update.go`, `restart.go` | Release update + restart |

### 7.1 The deploy flow

```
deploy job
  ├─ deployLk.TryAcquire(site)          reject if another deploy runs
  ├─ siteDirPath + siteRepoPath         validated paths
  ├─ cloneRepo(repoDir, token, sha)     shallow clone or fetch+checkout
  ├─ env.example -> .env if missing
  ├─ writeEnvFile(params.env)           resolveEnvPath rules apply
  ├─ write .ekilie-deploy               mode 0755
  └─ bash .ekilie-deploy (cwd repoDir)  stdout/stderr -> LogBatcher
```

### 7.2 The path rules

File: `internals/jobengine/validate.go`.

```
site_name  ->  ^[a-z0-9][a-z0-9-]{0,62}$     + containment under /opt/ekilie/sites
daemon name->  ^[a-z0-9][a-z0-9_-]{0,62}$    + containment under supervisor conf dir
env_path   ->  relative only, must stay inside <site>/current
```

Validation runs twice on purpose: `Execute` rejects bad params for site-scoped
actions before any work starts, and every path builder validates again defensively.
Anything that reaches `os.RemoveAll`, nginx config files, or supervisor program names
has passed both checks.

---

## 8. Scheduling loops

File: `internals/agent/agent.go`, `heartbeat.go`.

| Loop | Cadence | Job |
|---|---|---|
| `heartbeatLoop` | 30s | Collect metrics, send over WS or HTTP, update local `last_heartbeat` |
| `httpPollLoop` | poll interval (5s while WS is up) | Fetch pending jobs, dedup, dispatch |
| `updateCheckLoop` | immediate, then 24h | Check GitHub, self-update, restart |

The heartbeat payload includes CPU, memory, disk, load average, host uptime (with
agent process uptime alongside it), hostname, platform, kernel arch, and agent
version. Capabilities are probed once at startup (nginx, node, npm, docker, certbot,
git, systemctl, php, composer) and sent during registration.

---

## 9. Self-update and restart

Files: `internals/jobengine/self_update.go`, `restart.go`, `internals/agent/agent.go`.

```
CheckForUpdate(repo, current)
  └─ GET api.github.com/repos/<repo>/releases/latest
     compareVersions(tag, current) > 0  ->  update available

SelfUpdate(repo, release)                      [guarded: one at a time]
  ├─ pick ekilied-<os>-<arch>.tar.gz asset
  ├─ download archive + checksums.txt
  ├─ verify SHA256 (streamed)
  ├─ extract, run <new binary> --version        smoke test
  └─ swap: self -> self.bak, new -> self

RestartAgent(ctx)
  ├─ under systemd?  systemctl --no-block restart ekilied
  └─ otherwise       syscall.Exec(self, os.Args, os.Environ())
```

The ordering rules are subtle and were fixed deliberately:

- `--no-block` means the restart is queued, so the job success report is sent before
  systemd stops the process.
- Without systemd, `syscall.Exec` never returns on success, so the report must be
  sent before the re-exec.
- A restart failure is a failed job, not a silent success, and the old binary keeps
  running until the next check.

---

## 10. Security model

The daemon runs as root, so the trust boundaries are explicit.

| Boundary | Control | Where |
|---|---|---|
| Control plane input | Path validation + containment | `validate.go` |
| Job params | Claim before execute, per-action validation | `jobengine.go` |
| Panics | Recover per job, report failed, keep serving | `jobengine.go` |
| Error size | 8KB command tail, 4KB reported error | `helpers.go` |
| Credentials | `agent.yml` mode 0600, atomic writes | `config.go` |
| Update integrity | SHA256 checksums, smoke test | `self_update.go` |
| Docker streaming | 5 streams, tail and line limits | `logstream.go` |

Known gaps are tracked in the repository issue tracker. The most important ones for a
new reader: release artifacts are checksum-verified but not signed, the session token
is still passed in the WebSocket URL query string, and command-level hardening
(allowlists) is still open.

---

## 11. Failure playbook

| Symptom | Likely cause | Where to look |
|---|---|---|
| Jobs stop arriving | WS down and poll failing | `connectOnce`, `PollJobs` |
| Job stuck `pending` | Claim 409 or agent offline | `claimJob`, control plane |
| Logs missing at the end | Batcher close path | `LogBatcher.Close` |
| Duplicate execution | Dedup maps cleared early | `active`/`dispatched` |
| Agent dead after restart | Session not persisted | `SaveSession`, `restoreSessionFromDB` |
| Update repeats forever | Restart failed silently | `RestartAgent`, `finishSelfUpdate` |
| Docker listing crashes | Malformed container entry | `containerToInfo` |
| Traversal attempt | Invalid `site_name` or `env_path` | `validateActionParams` |

---

## 12. Tests as documentation

The tests encode the invariants that are easy to break.

| File | Pins down |
|---|---|
| `logbatcher_test.go` | Final flush, sequence monotonicity, writer sharing |
| `websocket_test.go` | 409 mapping, heartbeat and container list wire formats |
| `jobtrigger_test.go` | Claim before complete, 409, timeouts, panic recovery, no params marshal |
| `restart_test.go` | systemd vs re-exec ordering, no false success |
| `self_update_test.go` | Update concurrency guard |
| `update_test.go` | Immediate startup check, loop semantics |
| `logstream_test.go` | Async streams, stop, connection-drop cancellation |
| `validate_test.go` | Traversal rejection, env_path containment, filesystem safety |
| `helpers_test.go` | Streaming and bounded errors |
| `docker_test.go` | Container conversion edge cases and fuzzing |
| `config_test.go` | YAML round-trip, session save, mode 0600 |
| `main_test.go` | Session restore from the local database |

---

## 13. A suggested reading order

If you have one day, read in this order and answer the question at each step.

| Order | Read | Question to answer |
|---|---|---|
| 1 | `cmd/ekilied/main.go` | How does a fresh agent get a session token? |
| 2 | `internals/agent/agent.go` | Which loops run, and what stops each one? |
| 3 | `internals/agent/websocket.go` | How does one connection work, and what happens on drop? |
| 4 | `internals/jobengine/jobengine.go` | How is a job claimed, deduplicated, run, and completed? |
| 5 | `internals/jobengine/validate.go` | What input can reach the filesystem? |
| 6 | `internals/jobengine/helpers.go` | How is command output handled and bounded? |
| 7 | `internals/jobengine/self_update.go` + `restart.go` | How does the agent update itself safely? |
| 8 | `internals/agent/logstream.go` | Why is log streaming not part of the dispatch loop? |
| 9 | `internals/config/config.go` | What is the configuration precedence? |
| 10 | The test files | What behavior is guaranteed, and how? |

---

## 14. Glossary

| Term | Meaning |
|---|---|
| Control plane | The Ekilie Cloud API the agent talks to |
| Agent | This daemon, `ekilied` |
| Job | One unit of work with an action and params |
| Claim | Atomically marking a job as accepted before running it |
| Action | The switch case that implements a job, for example `deploy` |
| Trigger | The WS or poll message that says a job exists |
| Session token | Long-lived credential for agent API calls |
| Registration token | Single-use credential that creates the session |
| Egress | Outbound queues (`egress` high, `egressLow` low) |
| Batcher | The buffered log sender for one job |
| Lapse | A BYO management subscription ending (control-plane concept) |
