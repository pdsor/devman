# devman.yaml reference

One canonical spelling per concept. Unknown fields are rejected with
`CONFIG_INVALID` rather than ignored, so a typo is never silently dropped.

All paths are relative to the project root (or to `cwd`, where noted) so the file
works unchanged on Windows, macOS and Linux.

## Top level

```yaml
version: 1                  # required, only 1 exists
project:                    # required
  name: myapp               # required; [A-Za-z0-9][A-Za-z0-9._-]{0,63}
  display_name: My App      # optional, cosmetic
  description: ...          # optional
services:                   # required, at least one
  <name>: { ... }
startup:
  default: [frontend, backend]   # what a bare `devman start` starts
profiles:
  full: [frontend, backend, worker]
  minimal: [backend]
```

Without `startup.default`, a bare `devman start` starts the services marked
`autostart: true`; if none are, it starts all of them.

## Service

```yaml
services:
  backend:
    display_name: Backend       # cosmetic
    runtime: host               # host | docker-compose | external (default host)
    cwd: ./server               # relative to the project root, default "."
    command: python             # a STRING, never a mapping
    args: [-m, uvicorn, app.main:app, --port, "${PORT}"]
    shell: false                # true, or {type: powershell}
    env_file: [.env, .env.local]   # relative to cwd, applied in order
    env:
      NODE_ENV: development
    required_env: [DATABASE_URL]   # missing → BLOCKED with ENV_MISSING
    ports:
      - name: http
        value: auto             # "auto" or a fixed integer
        preferred: 8000         # only with value: auto
        range: backend          # named range from global settings
        env: PORT               # variable DevMan injects the number into
    depends_on:
      database:
        condition: healthy      # started | healthy (default started)
    health:
      type: http                # process | tcp | http
      url: http://127.0.0.1:${PORT}/health
      expected_status: [200]
      interval: 5s
      timeout: 2s
      retries: 3
    restart:
      policy: on-failure        # no | on-failure | always
      max_attempts: 3
      delay: 1s
      max_delay: 30s
    autostart: true
    graceful_timeout: 10s
    platform:
      windows:
        command: npm.cmd
        args: [run, dev]
        cwd: ./server
        env: { PYTHONUTF8: "1" }
      macos: { ... }
      linux: { ... }
```

### Rules the validator enforces

- `command` is a string. A mapping (`command: {default: pnpm, windows: pnpm.cmd}`)
  is `CONFIG_INVALID`; use `platform.<os>.command`.
- `ports` is a list. There is no `port:` singular form.
- `shell: true` and `args` together are `CONFIG_INVALID`: with a shell, the whole
  line lives in `command`, because splitting a shell line would change what its
  quotes and pipes mean.
- `preferred` is only meaningful with `value: auto`. A fixed `value` is never
  silently moved — if it is taken, the start fails with `PORT_CONFLICT`.
- `health.url` is required for `type: http`, `health.port` for `type: tcp`.
- `depends_on` must name existing services and must not form a cycle.
- `cwd`, every `env_file` and a `compose.file` must exist on disk.
- `command` must be resolvable on PATH (or be an existing path) — otherwise
  `COMMAND_NOT_FOUND` at validation time, before anything runs.
- Durations are `10s`, `500ms`, `2m`, or a bare integer meaning seconds.

## Runtimes

**host** — DevMan spawns and owns the process tree. Stopping it takes down
grandchildren too.

**docker-compose** — DevMan runs `docker compose up -d <service>` and follows the
container. Requires `compose`:

```yaml
    runtime: docker-compose
    compose:
      file: docker-compose.yml    # default
      service: postgres           # required: the compose service name
      project: myapp              # optional, `-p`
```

Missing Docker is `DOCKER_NOT_FOUND` → `BLOCKED`, never `FAILED`.

**external** — something else started it; DevMan only watches. It starts and
stops nothing, and its declared ports are *adopted* rather than reserved, so
`value: auto` is rejected: state the port that is already in use.

## Ports and injection

`value: auto` means DevMan chooses, records and injects the port. The service
must read the variable named in `env`:

- Node: `process.env.PORT`
- Python: `os.environ["PORT"]`, or `--port ${PORT}` in `args`
- Go: `os.Getenv("PORT")`

A service that ignores the injected variable stays `RUNNING` but its allocation
is marked `UNVERIFIED`, which is the signal that the wiring is missing.

Named ranges come from the global settings (`devman config list`); the defaults
are `frontend` 3000–3999, `backend` 8000–8999, `general` 10000–19999.

## Templates

Usable in `command`, `args`, `env` values, `health.url` and `health.port`:

| Variable | Meaning |
| --- | --- |
| `${PORT}` | the service's primary port |
| `${PORT:name}` | a specific declared port |
| `${PROJECT_DIR}` | absolute project root |
| `${SERVICE_DIR}` | absolute resolved `cwd` |
| `${HOME}` | user home |
| `${ENV:NAME}` | a variable from the user env layers |

`${ENV:PORT}` is rejected: an allocated port is only reachable through `${PORT}`,
which keeps the resolution order acyclic.

## Environment precedence

daemon environment → `env_file` in order → `env` → `platform.<os>.env` → DevMan
injection.

Injection wins deliberately: declaring `ports: [{value: auto, env: PORT}]` states
that DevMan owns `PORT`, so a stale `PORT=3000` in `.env` must not override the
port that was actually reserved.

`${ENV:NAME}` sees the user layers only, never injection.

## Health

No `health:` block means process-level health, reported as `N/A`. DevMan never
infers a probe from the presence of a port — a service being alive and a service
being ready are different claims, and only the project knows how to check the
second one.

`type: process` is alive-only. `tcp` connects. `http` requests `url` and compares
against `expected_status`.

## Restart

`policy` applies only while the desired state is RUNNING, so `devman stop` can
never lose a race with an automatic restart. Backoff is exponential with a cap
and jitter; a service that stays up for 60 s gets its budget reset. Exhausting
`max_attempts` ends in `FAILED` without rewriting the desired state.
