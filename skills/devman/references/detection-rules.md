# Detection rules

The goal is a `devman.yaml` that runs the commands the project *already*
documents. Read evidence first; infer only what the evidence leaves open.

## Read these, in this order

```text
README.md / CONTRIBUTING.md   documented dev commands, ports, prerequisites
package.json                  scripts, workspaces, package manager
pnpm-workspace.yaml, turbo.json, lerna.json, nx.json
pyproject.toml, requirements.txt, manage.py, alembic.ini
go.mod, main.go, cmd/
Cargo.toml
docker-compose.yml / compose.yml
Makefile, Taskfile.yml, Justfile, Procfile
.env.example, .env.sample
```

A command written in the README beats a command you derived from a config file.
If the two disagree, follow the README and say so.

## Identifying services

One service per independently startable long-running process. Typical shapes:

- `frontend` — a dev server with HMR (Vite, Next, CRA, Nuxt, Angular)
- `backend` / `api` — an HTTP server (Express, FastAPI, Django, Gin, Rails)
- `worker` — a queue consumer (Celery, BullMQ, Sidekiq, RQ)
- `database` / `cache` — from `docker-compose.yml`, as `runtime: docker-compose`

Not services: build watchers that only produce files, one-shot migrations, test
runners, linters. Those stay short-lived commands the agent runs directly.

Name services after their role (`frontend`, `backend`, `worker`), not after the
framework. In a monorepo, use the workspace name when it is clearer
(`web`, `admin`, `api-gateway`).

## Node

Package manager, from the lockfile — never guessed:

- `pnpm-lock.yaml` → `pnpm`
- `yarn.lock` → `yarn`
- `bun.lockb` → `bun`
- `package-lock.json` (or nothing) → `npm`

Command shape:

```yaml
command: npm
args: [run, dev]
platform:
  windows:
    command: npm.cmd        # npm/pnpm/yarn/npx are .cmd shims on Windows
```

Script choice: `dev` > `start:dev` > `serve` > `start`. Ignore `build`,
`preview`, `test`, `lint`.

Ports:

- Vite / Nuxt / Astro read `PORT`, and a `--port` flag wins over it. If
  `vite.config.*` hardcodes `server.port`, either declare that as a fixed port or
  pass `args: [run, dev, --, --port, "${PORT}"]` and let DevMan allocate.
- Next.js dev reads `PORT`; `next dev -p 3000` overrides it.
- CRA (`react-scripts`) reads `PORT`.
- Express and friends: whatever the code reads, usually `process.env.PORT`.

Monorepos: run each workspace as its own service with `cwd` set to the package,
or use the root script if the repo documents one. `pnpm --filter web dev` also
works, but per-package `cwd` gives clearer logs and independent restarts.

## Python

Interpreter and invocation:

- `.venv/` or `venv/` present → prefer the venv interpreter explicitly:
  `command: ./.venv/bin/python` with `platform.windows.command:
  .\.venv\Scripts\python.exe`. Otherwise `python`, or `python3` where that is what
  the project documents.
- Poetry (`[tool.poetry]` in `pyproject.toml`) → `command: poetry`,
  `args: [run, ...]`
- uv (`uv.lock`) → `command: uv`, `args: [run, ...]`

Common frameworks:

```yaml
# FastAPI / Starlette
args: [-m, uvicorn, app.main:app, --reload, --port, "${PORT}"]
health: { type: http, url: "http://127.0.0.1:${PORT}/health" }

# Django
args: [manage.py, runserver, "127.0.0.1:${PORT}"]

# Flask
env: { FLASK_APP: app.py, FLASK_DEBUG: "1" }
args: [-m, flask, run, --port, "${PORT}"]

# Celery worker — no port, no HTTP health
args: [-m, celery, -A, app.worker, worker, --loglevel=info]
```

Uvicorn and Django do **not** read `PORT` by themselves: pass `${PORT}` on the
command line. Set `env: { PYTHONUNBUFFERED: "1" }` so output is captured
immediately instead of sitting in a buffer.

## Go

```yaml
command: go
args: [run, ./cmd/server]
env: { PORT: "" }        # do not set it; the ports block injects it
```

Read the code for how the port is chosen — Go projects commonly use a flag
(`-addr :8080`) rather than an env var. If it is a flag, pass `${PORT}` to it.

## Docker Compose

For infrastructure the project expects to be running (Postgres, Redis, MinIO),
declare one service per compose service:

```yaml
  database:
    runtime: docker-compose
    compose: { file: docker-compose.yml, service: postgres }
    health: { type: tcp, port: "5432" }
```

Do not port-allocate a compose service: the port mapping lives in the compose
file, and DevMan does not rewrite it. Declare it as a fixed `value` only if
something else needs to know the number.

## Working directory

`cwd` is where the command would be run by hand. For a monorepo package that is
the package directory. `env_file` paths are relative to `cwd`, not to the project
root — a `.env` next to `package.json` in `apps/web` is `env_file: [.env]` with
`cwd: ./apps/web`.

## Dependencies

Declare `depends_on` only for a real ordering requirement:

- backend needs the database → `condition: healthy`
- frontend needs the backend's URL at boot → `condition: healthy`
- frontend only calls the backend from the browser at runtime → no dependency;
  ordering it would just make startup slower

`condition: healthy` requires the dependency to have a real probe. Without one it
is satisfied immediately, which is honest but not what the author usually meant —
so add the probe, or use `condition: started`.

## Environment

`.env.example` lists what the project needs. Copy the *names* into
`required_env` when a value must exist for the service to work at all
(`DATABASE_URL`, `STRIPE_SECRET_KEY`). Never copy example secrets into
`devman.yaml`, and never commit real ones — that is what `env_file` is for.

If `.env` does not exist but `.env.example` does, tell the user to create it
rather than guessing values.

## Health

Add a probe only when the project has an endpoint that means "ready":

- an explicit `/health`, `/healthz`, `/api/health`, `/-/health` route in the code
- Django with `django-health-check` installed
- a documented readiness path

Otherwise leave `health:` out. Process-level health reported as `N/A` is
truthful; a probe against a route that does not exist makes a working service
look broken forever.

Frontend dev servers rarely need a probe: `tcp` on the injected port is usually
the most a Vite server will honestly answer.

## Restart

Default to no restart policy. Add `on-failure` with a small `max_attempts` for a
worker that legitimately dies on a bad message. Do not add `always` to a service
whose crashes the user needs to notice.
