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

## M7 — Daemon API, events and reconciliation — done

`internal/daemon` is the background service: an HTTP API on loopback plus the
discovery, authentication and reconciliation around it. `internal/client` is the
only way anything above it (CLI now, GUI and Codex skill later) reaches DevMan
state, so the API contract and its error decoding exist in exactly one place.

Discovery and credentials:

- `daemon.json` holds `{pid, port, host, started_at, api_version, version,
  graceful_signals}`. The auth token lives in a separate `auth-token` file with
  0600 on Unix and a real user-only DACL on Windows, because Go's 0600 there
  only toggles the read-only attribute. A test asserts the token never appears
  in `daemon.json`.
- A stale record is deleted rather than reported: liveness requires both that
  the recorded PID exists *and* that something answers on the port, since PIDs
  are reused.
- The daemon port is found by actually binding across 39100–39149, so two
  daemons can never agree on one port — the loser simply fails to bind. A second
  daemon on a live record is refused with `ALREADY_RUNNING`.

Security boundary:

- Every request needs `Authorization: Bearer <token>`, compared in constant
  time. The two SSE endpoints also accept `?token=`, because the browser
  EventSource API cannot set headers; nothing else does.
- Origin is validated with no permissive CORS anywhere: only loopback pages and
  the Tauri shell are accepted, and a refused origin is never echoed back. A
  page on the internet must not be able to drive a local daemon that can start
  processes.
- `UNAUTHORIZED` now maps to 401 (a credential problem) and `PROJECT_UNTRUSTED`
  to 403 (understood and deliberately refused), which are different situations
  and were previously the same status.

API surface: daemon status and shutdown, paths, settings get/set, tool
resolution, project list/register/inspect/resolve/trust/validate/unregister,
project and service start/stop/restart, service status, log history and two
*separate* SSE endpoints — `/events/stream` for the daemon and
`/services/{name}/logs/stream` for one service, so a GUI watching one service's
output does not have to filter the whole daemon's traffic. Events are persisted
as they are published, so history survives a restart and a reconnecting client
can catch up.

Crash recovery, and a conflict that had to be resolved:

- The plan asked for both a Windows Job Object with
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` (D20) and adoption of services that
  outlive a daemon crash (D25). On Windows these are incompatible: the job dies
  with the daemon and takes the tree with it. The ruling was to **keep
  KILL_ON_JOB_CLOSE**, so Windows never leaves orphans and adoption is a Unix
  path; Windows reconciliation records the process as gone and releases its
  ports.
- `devman daemon stop` therefore **stops every service first**. Shutdown calls
  `Supervisor.StopAll`, which stops services in parallel so their graceful
  timeouts do not add up. `Supervisor.Close` only tears down monitoring, keeping
  "stop watching" and "stop the services" as separate decisions.
- `Supervisor.Reconcile` runs before the API accepts traffic, so the first
  status call reflects reality. A surviving process is adopted with
  `status: RUNNING` plus `observability.log_capture: detached` — the pipes died
  with the previous daemon, and saying so is more useful than inventing a new
  status. Adoption re-checks the identity on every poll, so a recycled PID is
  treated as an exit rather than silently supervising a stranger. A vanished
  service is recorded as CRASHED when it was meant to be running, and is never
  auto-started: reconciliation reports the truth, it does not make decisions the
  user did not ask for.

Verified end to end against a real listener: unauthenticated and wrong-token
requests are refused, a foreign origin is refused while a loopback origin is
echoed, the full service lifecycle works through the client (start → port and
URL → healthy → captured logs → port usage names the owner → stop releases every
reservation), events arrive on the SSE stream and are also queryable in
chronological order afterwards, `shutdown` stops the service process, an invalid
settings edit is refused rather than persisted, and a stale `daemon.json` is
cleaned up.

## M8 — CLI and acceptance suites — done

`internal/cli` plus the three-line `cmd/devman/main.go` complete the core. The
CLI is a presentation layer: every piece of state it prints comes from a DTO the
daemon returned, so `--json` is the contract and the tables are a rendering of
it. Nothing in DevMan parses terminal text to discover state, which is what lets
the GUI and the Codex skill behave identically later without reimplementing
anything.

- `init`, `validate`, `paths` and `version` run locally on purpose. A user asking
  where the data lives is often asking precisely because the daemon will not
  start. Everything else auto-starts the daemon, so a user never has to know it
  exists; `daemon status` and `daemon stop` deliberately do **not** auto-start,
  because a status command that starts what it reports on can never tell you it
  was stopped.
- `register` prints project, location, per-service runtime, command line, shell,
  cwd, `env_file` and compose target, then asks for confirmation. With `--json`
  and no `--trust` it prints the same preview and refuses with
  `PROJECT_UNTRUSTED`: a non-interactive caller must never be prompted and must
  never have trust granted implicitly on its behalf.
- Flags are accepted after positional arguments. Go's `flag` package stops at the
  first non-flag argument, so `devman logs backend --follow` would have silently
  ignored `--follow`; arguments are permuted before parsing, with `--` preserved
  so a positional that begins with a dash still survives.
- `start`/`stop`/`restart` always route through the project endpoint even when
  named services are given, because dependency ordering is a project-level fact:
  starting `backend` may require starting `database` first, and only the daemon
  knows that. Per-service failures are printed under the table and produce a
  non-zero exit without hiding the services that did come up.
- `--wait` polls until every affected service is healthy or terminal. A RUNNING
  but not yet healthy service is explicitly *not* settled, because a dev server's
  first probe usually fails and returning then would report a working service as
  broken.
- Status output annotates what changes its meaning rather than burying it:
  `(adopted)`, `(no log capture)`, `×N` restarts, and a `?` on a port the service
  never bound. `logs` warns before printing nothing when a service was adopted
  and its output is no longer captured.
- `init` emits only canonical spellings — `ports:` as a list, no `port:` sugar,
  `platform.<os>.command` for per-OS differences — and never invents a health
  probe, since the scaffold is the example most users copy from.

**Acceptance suites** (`internal/acceptance`) drive the real CLI against a real
daemon over the real HTTP API; the only concessions to a test environment are a
temporary data directory and a private daemon port range.

1. *Full chain*: registering without approval is refused; `register --trust` →
   `start --wait` brings up backend and frontend in dependency order, both
   RUNNING and HEALTHY with ports observed BOUND and a URL; `status --json`,
   `logs`, `restart` (new PIDs) and `stop` all work; every process in the tree is
   gone afterwards and every reservation released.
2. *Concurrency*: two projects that both prefer 3000 both start, on 3000 and
   3001, and neither `devman.yaml` is modified — DevMan resolves conflicts at
   runtime, it does not rewrite a user's file.
3. *Crash recovery*: a service that outlives its daemon is adopted by the next
   one with the same PID, reported RUNNING with `log_capture: detached` and its
   port still accounted for; restarting replaces the process and restores
   capture. On Windows the job object normally takes the tree down with the
   daemon, so the suite skips rather than pretends when there is no survivor.

Running everything in parallel surfaced one real bug, now fixed with a
regression test: `events.Bus.Publish` snapshotted its subscriber channels,
released the lock and then sent, so a health probe landing during shutdown could
send on a channel `Close` had just closed and crash the daemon. The fan-out now
happens under the lock, which is safe because every send is non-blocking, and
publishing after `Close` is recorded rather than delivered. Each test package
also got its own port window, since `go test ./...` runs packages concurrently
against separate databases and two suites sharing the default range would be
handed the same free port.

## M11 — Codex / agent skill — done

`skills/devman/` is the skill an agent loads instead of learning DevMan by trial
and error. It is documentation, not code: the CLI already exposes everything, so
the skill's job is to make an agent use it correctly.

- `SKILL.md` leads with the rule the whole product exists for: never run a
  long-lived dev command in your own shell. It then gives the decision procedure
  (`devman status --json` first, branch on the error code), the onboarding
  sequence, how to read a failure, and the full command list.
- The `--trust` flag is documented as what it is: approval recorded on the user's
  behalf. The skill instructs the agent to let the *user* answer the interactive
  prompt when there is one, because a `devman.yaml` is executable code.
- `references/schema.md` is the field reference including the rules the validator
  enforces, so a generated config is right the first time.
- `references/detection-rules.md` says to read the repository before writing
  anything, and to prefer a documented command over an inferred one. It covers
  Node package-manager detection from the lockfile, Python venv/Poetry/uv
  invocation, Go's flag-instead-of-env habit, monorepos and compose services.
- `references/examples.md` carries complete configurations, including the honest
  limitation that one service's auto-allocated port is not visible in another
  service's template, so cross-service URLs need a fixed port.
- `references/errors.md` maps every code to the action it implies, and contrasts
  BLOCKED (prerequisite missing, nothing attempted) with FAILED and CRASHED.
- `scripts/devman-check.ps1` and `scripts/devman-check.sh` answer "is DevMan
  here, is there a config, is it valid, is it registered, is it running" as one
  JSON object with a `next_action`. The script deliberately starts nothing: a
  check that launches a daemon as a side effect is a surprise.

Verified against a real Node project end to end: the check script reports `init`
with no config, `query_status` with a valid config and no daemon, and
`register` → `start` → `nothing` as the documented flow proceeds, with the
service reaching RUNNING/HEALTHY on an auto-allocated port.

## M9/M10 — desktop GUI — done

`apps/desktop/` is a Tauri 2 shell (React 19 + Vite 8 + TypeScript) over the same
HTTP API the CLI uses. The window holds no DevMan logic of its own: it renders
`pkg/dto` and calls endpoints, so there is no second implementation of state to
drift out of sync.

Rust does only the two things a webview cannot: read the daemon's discovery file
and token from disk (`daemon_endpoint`), and start the daemon detached when it is
not running (`start_daemon`). `devman_paths` duplicates the Go layout rules
deliberately — the alternative is asking a daemon that may be down where its own
files are.

The frontend layers are `src/api/` (types mirroring the DTOs, a fetch client that
decodes the error envelope into an `ApiError` carrying the daemon's own code, and
the Tauri bridge), `src/hooks.ts` for loading, and `src/pages/`.

Refresh is driven by the event stream rather than by polling: one SSE connection
feeds the whole window through `FeedContext`, and a page reloads when an event
that concerns it arrives. Only uptime and port bindings — which have no event of
their own — poll, and slowly. Loaded data is kept while a reload is in flight,
because a dashboard that blanks on every event is unreadable exactly when it
matters, while services are booting.

Pages: Projects (cards with status, tally and quick start/stop), project detail
(per-service state, health, ports, dependency order, capture warnings and
start/stop/restart), Logs (live stream with stream and text filters), Ports
(allocations plus a "who has this port" lookup that also answers for foreign
processes), Activity (the raw event log merged with persisted history), Add a
project (inspect → show exactly what would execute → approve), Configuration
(edits the real `devman.yaml` through the new endpoints, with a refusal shown as
inline findings), Environment (tools resolved in the daemon's own process, which
is how a GUI launched with a reduced PATH gets diagnosed) and Settings.

Two rules carry through the interface. Anything the daemon reports is set in mono
and anything the interface says about itself is set in the display face, so
machine truth and chrome never look alike. And colour is only ever a status — the
palette *is* the status legend, not decoration. The signature element is a
strip-chart of recent events per service: each tick is a real event, coloured by
kind and as tall as it is significant, so a service that crashed and restarted
twice looks different at a glance from one that has been quietly running.

Trust is visible wherever it applies: an unapproved project cannot be started
from the GUI either, the disabled control says why, and saving an edit that
changes what a project executes reports that approval is needed again.

Supporting daemon work in the same milestone: `GET`/`PUT /projects/{id}/config`
serve and replace the project's actual configuration file. The PUT parses and
validates before writing and writes atomically, so an invalid edit is refused
with its findings and the file on disk is left exactly as it was.

Verified with `go test ./...`, `tsc --noEmit`, `vite build`, and `cargo build`
for the shell.

## GUI localisation — Chinese by default, English optional — done

The window speaks Chinese out of the box and can be switched to English from
Settings or from the connect screen. The choice is stored in `localStorage`
(`devman.locale`) and, on first run, taken from the browser locale — anything
that is not English falls back to Chinese, because that is the default the
project asked for.

`src/i18n/messages.ts` holds both dictionaries. The Chinese one is declared
`as const` and its keys *are* the key type (`export type MessageKey = keyof
typeof zh`); English is declared as `Record<MessageKey, string>`. A missing or
misspelled translation is therefore a compile error, not a string that silently
renders as a key — `tsc` caught one missing key while this was being written,
which is the whole point of the arrangement.

Daemon vocabulary is deliberately **not** translated. `RUNNING`, `BLOCKED`,
`UNVERIFIED`, `CONFIG_INVALID` and the rest stay exactly as they appear in
`--json`, in the events and in the docs; the Chinese explanation lives in the
tooltip beside them (`status.BLOCKED` → "BLOCKED：前置条件不满足，什么都没执行").
Translating them would mean the GUI and the CLI disagree about the name of a
state, and would break the one habit that makes DevMan diagnosable — searching
for the token you saw on screen.

Making this possible required extending the "clients render codes, not
sentences" rule all the way out to the Rust shell: `lib.rs` now returns
`NO_HOME`, `DAEMON_NOT_RUNNING`, `DISCOVERY_UNREADABLE`, `TOKEN_UNREADABLE`,
`DEVMAN_NOT_FOUND`, `SPAWN_FAILED: …` and `DAEMON_NOT_READY` instead of English
prose, and the fetch client reports a dead daemon as `DAEMON_NOT_RUNNING`
rather than a browser network message. `describeFailure` maps a code to a
sentence and appends whatever detail followed it; an **unknown** code is shown
verbatim rather than replaced with a generic apology, so a failure DevMan has
not anticipated is still readable.

Durations are assembled from unit keys rather than formatted in English and
patched, so `2 天 3 小时` and `2d 3h` are the same code path.

Verified with `tsc --noEmit`, `vite build` and a release `tauri build`.

## M12 — packaging and CI — done

Release tooling lives in `tools/build` and is written in Go, not as a Makefile
or a shell script. The reason is the same reason DevMan itself is a single
CGO-free binary: the tool that cuts a release should not need anything the
project does not already need. `go run ./tools/build dist` behaves identically
on Windows, macOS and Linux, so a release built on a laptop and a release built
by a runner go through the same code rather than through two dialects of the
same script.

Three targets: `version` prints what would be stamped, `dist` cross-compiles the
CLI for six targets (windows, darwin and linux × amd64 and arm64), archives each
one with the README and writes a `sha256sum -c` compatible `SHA256SUMS`, and
`sidecar` builds the CLI for the host and names it after the Rust target triple.

Versioning finally connects: `cmd/devman/main.go` has always documented
`-ldflags -X main.Version=…`, and nothing was passing it. The tool takes
`DEVMAN_VERSION` when CI sets it from the tag, otherwise `git describe`, and it
keeps the `-dirty` suffix on purpose — a binary built from uncommitted work
should not impersonate a tag. Verified end to end: the binary extracted from
`devman_…_windows_amd64.zip` reports that version through `devman --json
version`.

The installed desktop app now carries its own daemon. `tauri.conf.json` declares
`externalBin: ["binaries/devman"]`, and the sidecar target runs from
`beforeBuildCommand` and `beforeDevCommand`, so it is impossible to build a
bundle without one. This closes a real gap: `devman_executable()` in the shell
looks for a binary next to the application before falling back to `PATH`, and
that branch was unreachable — while a GUI launched from the Start menu or the
Dock is exactly the case where `PATH` cannot be trusted.

`.github/workflows/ci.yml` runs `go vet`, a tidiness check, the build and
`go test -count=1 ./...` on Linux, macOS and Windows, because process
supervision is where the platforms disagree and mocking the platform away would
test nothing. `-count=1` is deliberate: these tests assert on live process and
port state, and a cached PASS from another machine's state proves nothing. A
second job cross-compiles every release target so a broken target is found
before a tag is cut, and a third type-checks the frontend, bundles it and
`cargo check`s the Rust shell.

`.github/workflows/release.yml` fires on a `v*` tag: one job produces the CLI
archives and checksums, one produces the Windows installer with the sidecar
inside, and a third publishes them with `gh release create` — preinstalled on
the runners, so no third-party action needs write access to the repository.
macOS and Linux ship the CLI only for V0.1; bundling those desktop targets
means signing and notarisation, which is a decision about identity rather than
a build step.

## Python/FastAPI acceptance fixtures — done

The existing suites use the test binary as their service, which keeps them
hermetic but also keeps them Go-shaped. `internal/acceptance/python_test.go`
covers the three ways a Python service actually differs:

- Uvicorn and Django do not read `PORT`. The port must arrive as a command line
  argument, so the fixture declares its port with **no** `env:` key and passes
  `--port ${PORT}` in `args`. A `BOUND` port is then proof that template
  expansion reached the argument list, not just the environment.
- CPython block-buffers stdout when it is a pipe. The fixture prints its startup
  line without an explicit flush and relies on `PYTHONUNBUFFERED: "1"` from
  `devman.yaml`; if the env layer did not reach the process the line would sit
  in a buffer for as long as the service runs, and the assertion would time out.
- Stopping the service goes through the interpreter's signal handling rather
  than Go's runtime, including `SIGBREAK` on Windows.

`TestPythonService` needs only a stdlib interpreter. `TestFastAPIService` runs
the configuration from the documentation against real FastAPI and Uvicorn and
also asserts that Uvicorn's banner — which it writes to stderr — is captured.
It skips rather than installing anything: a test that reaches the network to
pass fails for reasons that have nothing to do with DevMan. CI installs the two
packages explicitly and points the suite at that interpreter with
`DEVMAN_TEST_PYTHON`, so the fixture runs on all three platforms instead of
quietly skipping.

Verified locally against FastAPI 0.141.1 and Uvicorn 0.52.4 in a throwaway
virtual environment, and `go test -count=1 ./...` is green with it included.

## Next

- V0.1 release candidate: tag, then verify the installer on a clean machine
- macOS and Linux desktop bundles once signing identities exist




