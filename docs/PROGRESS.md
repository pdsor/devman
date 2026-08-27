# V0.1 progress

Core before GUI. Each milestone must be buildable, tested and usable before the
next one starts.

## M1 — Repository scaffold, config types, schema, tests — done

- `pkg/errs`: unified error codes shared by the CLI, the HTTP API and the Codex
  skill, with an HTTP status mapping.
- `pkg/config`: canonical `devman.yaml` parser. Unknown fields are rejected.
  One canonical spelling per concept: `ports:` is always a list, `command:` is
  always a string, platform differences live in `platform.<os>`. Both the list
  and the mapping form of `depends_on` normalise into one ordered structure.
- Validation covers schema, runtimes, shell/args conflicts, port ranges and
  static collisions, dependency existence and cycles, health and restart
  shapes, template variables, `cwd`/`env_file`/compose file existence and PATH
  resolution of `command`. Output is a machine readable `ValidationResult` for
  `devman validate --json`.
- Template engine for `${PORT}`, `${PORT:name}`, `${PROJECT_DIR}`,
  `${SERVICE_DIR}`, `${HOME}` and `${ENV:NAME}`. `${ENV:PORT}` is rejected on
  purpose: DevMan-allocated ports are only reachable through `${PORT}`.
- `ExecutionFingerprint`: a hash over execution-relevant fields only (runtime,
  cwd, command, args, shell, env_file, env, compose, platform overlays, service
  set). Cosmetic edits such as `display_name` or a health interval never
  invalidate trust; anything that changes what runs does.
- `internal/paths`: OS-convention data directory with a `DEVMAN_HOME` override.
- `internal/settings`: global `config.yaml` with the daemon port scan range,
  named port ranges, log rotation, and defaults for graceful timeout, health
  and restart backoff. `daemon.host` is restricted to loopback.

## M2 — Process supervisor and cross-platform process tree — done

`internal/platform` owns spawning, signalling and killing.

- Windows: every service gets a Job Object with
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, and the child is started
  `CREATE_SUSPENDED` so it is inside the job before it can spawn anything.
  Threads are then resumed explicitly. Stop sends `CTRL_BREAK` to the child's
  own process group (`CREATE_NEW_PROCESS_GROUP`), waits, then calls
  `TerminateJobObject`. The daemon calls `EnsureConsole` at startup because
  `GenerateConsoleCtrlEvent` cannot reach a child without a console; console
  detection uses `GetConsoleCP` since a ConPTY console has no window.
- Unix: a new process group per service, `SIGTERM` then `SIGKILL` to `-pgid`.
- Output goes through explicit `os.Pipe`s instead of `exec`'s copying
  goroutines, so waiting for exit can never block on a detached grandchild that
  inherited the pipe.
- `Identity` (pid, spawn time, executable, command fingerprint) is recorded at
  spawn time, not at reconciliation time, and `Inspect`/`MatchesIdentity`
  reject recycled PIDs using process start time.
- `StopOutcome` distinguishes a graceful exit from a force kill, including the
  case where the graceful signal could not be delivered at all.

Verified on Windows: streams stay separate, exit codes propagate, a graceful
CTRL_BREAK shutdown returns the child's own exit code, a signal-ignoring
process is force-killed, and stopping a service kills a grandchild that
deliberately outlives its parent. Linux and macOS currently compile and vet
clean; the same integration tests run on all three platforms in CI.

## M3 — Log capture and status model — done

- `internal/logstore` turns raw output into structured records (seq, timestamp,
  project, service, stream, message). Records are appended as NDJSON so a
  restarted daemon can serve full-fidelity history, not just text.
- `LineWriter` splits streams into lines, handles CRLF and partial writes, and
  flushes bounded chunks when a service never emits a newline.
- Per-service ring buffer (2000 records) is warmed from disk on open, so
  history and live subscription need no file read per request. Sequence numbers
  continue across daemon restarts.
- Rotation is 10 MB with 5 backups by default; `History` transparently reads
  across rotated files when a caller asks for more than the ring holds.
- SSE subscribers get a buffered channel; a subscriber that stops reading is
  dropped records rather than being allowed to stall the service it watches.
- `LastErrors` serves the "what just broke" query an agent makes after a failed
  start.
- `pkg/dto` defines the stable API/CLI contract up front: process status,
  desired state, health, port allocation, project aggregate, process instance,
  events and daemon discovery info. `ProcessStatus` and `HealthStatus` are
  separate, and observability flags (`log_capture`, `adopted`) live outside the
  status enum so it does not inflate.

## M4 — Project registry on SQLite — done

- `internal/storage` uses the CGO-free `modernc.org/sqlite` driver, so DevMan
  stays a single cross-compilable binary. Tables: `projects`,
  `service_runtime`, `process_instances`, `port_allocations`, `events`, `meta`.
- Port exclusivity is enforced by a **partial unique index** on active
  allocations, not by an application lock. Ten goroutines racing for one port
  produce exactly one winner and nine `PORT_CONFLICT`s.
- `service_runtime` stores `desired_state` and `actual_state` side by side from
  the first version, so a manual stop cannot be undone by a restart policy,
  even across a daemon restart. It also stores process identity (pid, spawn
  time, executable, command fingerprint) plus `log_capture` and `adopted`.
- Unregistering a project cascades its runtime state and explicitly releases
  its ports, so nothing leaks until the next daemon restart.
- `internal/registry` implements registration, trust and the config cache.
  `Inspect` returns the execution summary the user must approve; `Register`
  takes an explicit trust decision. Trust is bound to the execution
  fingerprint: renaming a display name or retuning a health interval keeps a
  project trusted, while changing command, args, cwd, shell, env, env_file,
  runtime or compose target raises `PROJECT_UNTRUSTED` and blocks starting.
- Configuration is re-read only when mtime or size changes, so edits take
  effect immediately without a reload command and status calls stay cheap.
- Project ids are derived from the project path, so re-registering a directory
  keeps its logs and history.

## M5 — Port manager — done

`internal/portmgr` is the only component allowed to choose a port; every start
goes through `ReserveService`. There is no `findFreePort` helper anywhere else,
because that pattern is exactly how concurrent starts end up sharing a port.

- Allocation checks the registry *and* the OS, then claims the port with an
  INSERT that the partial unique index adjudicates. Ten services requesting
  auto ports concurrently receive ten distinct ports.
- Two projects that both prefer 3000 get 3000 and 3001 with no config edits.
- A fixed port is never silently moved: `value: 8000` in use fails with
  `PORT_CONFLICT`, flagged `fixed`, enriched with the holding PID and process
  name where the platform can resolve it.
- A multi-port service is all-or-nothing; a failure on the second port releases
  the first, so no half-allocated service leaks ports.
- Availability probing binds IPv4 loopback *and* IPv4 wildcard, since a service
  bound to only one of them would look free to a probe of the other. IPv6 is
  consulted only when this machine can actually listen on IPv6.
- `Verify` marks ports BOUND when a listener appears and UNVERIFIED when the
  service ignored its injected `PORT`. An unverified port never kills the
  process; health checking is what decides whether it matters.
- Owner lookup is best effort by design: Windows reads the TCP table via
  `GetExtendedTcpTable`, Linux matches `/proc/net/tcp` inodes to process fds,
  macOS shells out to `lsof` when present. An unknown owner is a warning, never
  a blocked start.

## M6 — Environment, health, runtimes, supervisor — done

This milestone turns the pieces into something that actually runs services.

**`internal/envresolve`** implements the environment precedence chain: daemon
environment → `env_file` in declaration order → `service.env` →
`platform.<os>.env` → DevMan injection. Injection wins on purpose: declaring
`ports: [{value: auto, env: PORT}]` states that DevMan owns that variable, so a
stale `PORT` in `.env` must not override the allocated one. `${ENV:NAME}` sees
the user layers only, never injection, which keeps the chain acyclic — a
template can never depend on a value that has not been allocated yet. The
resolver also handles the reduced PATH a GUI-launched daemon inherits, and the
absolute path it resolves is runtime state that is never written back into
`devman.yaml`.

**`internal/health`** keeps health strictly separate from process state: a
service can be RUNNING and UNHEALTHY, and that distinction is the point. Every
probe gates on "is the process alive" first, so a stale listener on the same
port cannot report a false success. An undeclared `health:` block means
`process`, reported as `N/A` — DevMan never infers a tcp or http probe from the
presence of ports.

**`internal/events`** is the structured event bus, in place from the moment the
supervisor exists rather than retrofitted when SSE arrives. Fan-out never
blocks: a subscriber that stops reading loses events instead of stalling the
service state change that is trying to be reported.

**`internal/runtime`** hides *how* a service executes behind one interface, so
the supervisor has no host-versus-Docker branching:

- `HostRuntime` spawns through the M2 platform layer, so the whole process tree
  stays contained. `shell: true` hands the line to `cmd.exe /D /S /C`,
  `pwsh -NoProfile -Command` or `/bin/sh -c` verbatim — splitting a shell line
  ourselves would silently change what quotes and pipes mean.
- `ComposeRuntime` calls `docker compose up -d <service>` and then follows the
  container's logs as a tracked process, which is also how liveness is observed.
  A missing Docker is `DOCKER_NOT_FOUND` → BLOCKED, not FAILED.
- `ExternalRuntime` starts and stops nothing. It exists so a service someone
  else launched can be listed, health checked and depended upon; terminating a
  process DevMan does not own is exactly the surprise a process manager must
  never produce. Its declared ports are *adopted* rather than reserved, because
  something is already listening on them.

**`internal/supervisor`** is the state machine:

- Desired state is written to SQLite *before* anything is spawned or signalled.
  A restart policy acts only while the desired state is RUNNING, so `devman
  stop` can never lose a race with an automatic restart, and a daemon crash
  mid-stop cannot resurrect a service.
- Every instance carries a generation. A stop bumps it before terminating, so
  the exit it causes is reported as a stop and never mistaken for a crash.
- A start resolves everything before creating a process: env layering,
  `required_env` gating, port reservation, template expansion, executable
  lookup, health spec. A missing prerequisite is BLOCKED (nothing is broken:
  install Docker, set the variable, free the port); a genuine failure is FAILED.
  Either way the ports reserved during a failed preparation are released.
- Restart backoff is exponential with a cap and up to 20% jitter, so dependent
  services do not retry in lockstep when a database comes back. A service that
  stayed up for a minute has its restart budget reset, and `max_attempts` ends
  in FAILED without rewriting the desired state.
- `depends_on` is honoured through `TopoOrder`, which also pulls in transitive
  dependencies — starting `backend` starts the database it declares, or the
  start is a lie. `condition: healthy` waits for a real probe; a probe-less
  dependency satisfies it immediately.
- A project start does not abort on the first failure: a broken worker must not
  stop the frontend from coming up. Services that depend on the failure are
  reported as `DEPENDENCY_FAILED` rather than launched into a broken
  environment. Stopping runs in reverse dependency order.
- Every state change is published as an event and annotated into the service's
  own log, so the reason a service stopped sits next to the output that preceded
  it.

Verified on Windows with the test binary as its own fixture: port injection
reaching the process and the allocation observed as BOUND, http health becoming
HEALTHY, a graceful stop preserving the service's own exit code and releasing
every reservation, an explicit stop never counting as a crash under
`restart: always`, `on-failure` retrying exactly `max_attempts` times and then
reporting FAILED, missing `required_env` and a missing executable blocking
without spawning or leaking ports, a dependent service waiting for its
dependency's health, and a service that ignores its injected port staying
RUNNING while its allocation is marked UNVERIFIED.

## Next

- M7 daemon API and events
- M8 CLI




