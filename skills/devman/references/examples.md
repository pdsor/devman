# Worked examples

Each of these validates as written, once the paths and commands match the real
project. Copy the shape, not the values.

## Vite frontend only

```yaml
version: 1

project:
  name: web-app

services:
  frontend:
    display_name: Frontend
    runtime: host
    cwd: .
    command: pnpm
    args: [dev]
    ports:
      - name: http
        value: auto
        preferred: 5173
        range: frontend
        env: PORT
    autostart: true
    platform:
      windows:
        command: pnpm.cmd
```

Vite reads `PORT`. If `vite.config.ts` pins `server.port`, either drop
`preferred` and declare that number as a fixed `value`, or pass the injected one
through: `args: [dev, --port, "${PORT}"]`.

## FastAPI backend + Postgres in Compose

```yaml
version: 1

project:
  name: api-service

services:
  database:
    display_name: Postgres
    runtime: docker-compose
    compose:
      file: docker-compose.yml
      service: postgres
    health:
      type: tcp
      port: "5432"
      interval: 2s

  backend:
    display_name: API
    runtime: host
    cwd: .
    command: ./.venv/bin/python
    args: [-m, uvicorn, app.main:app, --reload, --port, "${PORT}"]
    env:
      PYTHONUNBUFFERED: "1"
    env_file: [.env]
    required_env: [DATABASE_URL]
    ports:
      - name: http
        value: auto
        preferred: 8000
        range: backend
        env: PORT
    depends_on:
      database:
        condition: healthy
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 5s
      timeout: 2s
    autostart: true
    platform:
      windows:
        command: .\.venv\Scripts\python.exe
```

## Full stack, frontend needs the backend URL at build time

```yaml
version: 1

project:
  name: shop

services:
  backend:
    runtime: host
    cwd: ./server
    command: npm
    args: [run, dev]
    env_file: [.env]
    required_env: [DATABASE_URL]
    ports:
      - name: http
        value: auto
        preferred: 8080
        range: backend
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/api/health
    platform:
      windows: { command: npm.cmd }

  frontend:
    runtime: host
    cwd: ./web
    command: npm
    args: [run, dev]
    env:
      VITE_API_URL: http://127.0.0.1:${PORT:http}
    ports:
      - name: http
        value: auto
        preferred: 3000
        range: frontend
        env: PORT
    depends_on:
      backend:
        condition: healthy
    platform:
      windows: { command: npm.cmd }

startup:
  default: [backend, frontend]
```

`${PORT:http}` inside `frontend.env` resolves to *the frontend's own* `http`
port. To hand the frontend the backend's address, the backend needs a fixed port
(`value: 8080`) — DevMan does not expose one service's allocated port to another
service's template. That is a real limitation, and a fixed port with a
`PORT_CONFLICT` on collision is the honest way around it.

## Celery worker (no port, no HTTP health)

```yaml
  worker:
    display_name: Celery worker
    runtime: host
    cwd: .
    command: ./.venv/bin/python
    args: [-m, celery, -A, app.worker, worker, --loglevel=info]
    env:
      PYTHONUNBUFFERED: "1"
    env_file: [.env]
    required_env: [REDIS_URL]
    depends_on:
      redis:
        condition: healthy
    restart:
      policy: on-failure
      max_attempts: 3
    platform:
      windows:
        command: .\.venv\Scripts\python.exe
        args: [-m, celery, -A, app.worker, worker, --loglevel=info, --pool=solo]
```

No `ports` and no `health` block: the worker serves nothing, so process-level
health (`N/A`) is the truthful answer. The Windows overlay exists because Celery's
default prefork pool does not work there.

## Go service with a flag instead of an env var

```yaml
  api:
    runtime: host
    cwd: .
    command: go
    args: [run, ./cmd/server, -addr, "127.0.0.1:${PORT}"]
    ports:
      - name: http
        value: auto
        range: backend
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/healthz
```

`env: PORT` is still declared — it costs nothing and makes the allocation visible
to the process — but the flag is what the program actually reads.

## Monorepo with profiles

```yaml
version: 1

project:
  name: platform

services:
  api:
    runtime: host
    cwd: ./apps/api
    command: pnpm
    args: [dev]
    ports: [{ name: http, value: auto, preferred: 8000, range: backend, env: PORT }]
    health: { type: http, url: "http://127.0.0.1:${PORT}/health" }
    platform: { windows: { command: pnpm.cmd } }

  web:
    runtime: host
    cwd: ./apps/web
    command: pnpm
    args: [dev]
    ports: [{ name: http, value: auto, preferred: 3000, range: frontend, env: PORT }]
    depends_on: { api: { condition: healthy } }
    platform: { windows: { command: pnpm.cmd } }

  admin:
    runtime: host
    cwd: ./apps/admin
    command: pnpm
    args: [dev]
    ports: [{ name: http, value: auto, preferred: 3100, range: frontend, env: PORT }]
    platform: { windows: { command: pnpm.cmd } }

startup:
  default: [api, web]

profiles:
  full: [api, web, admin]
  backend-only: [api]
```

`devman start --profile full` starts all three; a bare `devman start` starts the
two in `startup.default`.

## A service someone else runs

```yaml
  legacy-api:
    display_name: Legacy API (external)
    runtime: external
    ports:
      - name: http
        value: 9000        # already in use; "auto" is rejected for external
        env: LEGACY_PORT
    health:
      type: http
      url: http://127.0.0.1:9000/status
```

DevMan starts nothing and stops nothing here. The service exists in status output
so other services can depend on it and so the user can see whether it is up.

## Multi-port service

```yaml
  api:
    runtime: host
    command: node
    args: [server.js]
    ports:
      - name: http
        value: auto
        preferred: 8000
        range: backend
        env: PORT
      - name: debug
        value: auto
        range: general
        env: INSPECT_PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT:http}/health
```

Multi-port allocation is all-or-nothing: if the second port cannot be reserved,
the first is released and nothing is started. With more than one port, `${PORT}`
still means the primary one, but naming it (`${PORT:http}`) is clearer.

## Shell line (only when the shell is genuinely needed)

```yaml
  pipeline:
    runtime: host
    shell: true
    command: "npm run build && npm run serve"   # no `args` with shell: true
```

Prefer `command` + `args` whenever possible: it avoids a shell, avoids quoting
rules that differ per platform, and gives a cleaner process tree.
