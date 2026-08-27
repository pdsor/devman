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

## Next

- M4 project registry on SQLite
- M5 port manager
- M6 health, dependencies, restart policy
- M7 daemon API and events
- M8 CLI


