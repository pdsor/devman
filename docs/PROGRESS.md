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

## Next

- M2 process supervisor and cross-platform process tree
- M3 log capture and status
- M4 project registry on SQLite
- M5 port manager
- M6 health, dependencies, restart policy
- M7 daemon API and events
- M8 CLI
