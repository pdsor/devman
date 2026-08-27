# DevMan

**AI-native Local Development Runtime Manager.**

DevMan owns the lifecycle of your local development services. Instead of a
dozen terminals and remembered commands, a project declares how it runs in
`devman.yaml`, and DevMan's daemon starts, stops, restarts, port-allocates,
health-checks and logs every service — for you and for AI coding agents alike.

```text
GUI / CLI / Codex Skill / future MCP
                 │
                 ▼
        DevMan Local API (127.0.0.1)
                 │
                 ▼
          DevMan Daemon
       ┌─────────┼──────────┐
    Process    Docker     Health
```

The daemon, not the AI agent and not your terminal, owns long-running
processes. Closing your editor, your terminal or the DevMan window does not
stop your services.

## Status

V0.1 in development. The core is built before the GUI, on purpose: a beautiful
dashboard over an unreliable process supervisor is worthless. See
[docs/PROGRESS.md](docs/PROGRESS.md).

## Repository layout

```text
cmd/devman/          single binary: CLI + daemon
internal/            daemon internals (process, port, health, log, storage, api)
pkg/config/          canonical devman.yaml schema, parser, validator
pkg/errs/            shared error codes used by the API, the CLI and the skill
schemas/             devman.schema.json for editor autocomplete
apps/desktop/        Tauri 2 desktop shell (React + TypeScript)
tools/build/         release tooling: cross-compilation, archives, sidecar
```


## Configuration

`devman.yaml` lives at the project root and is fully portable across Windows,
macOS and Linux. Paths are always relative to the project root.

```yaml
version: 1

project:
  name: my-ai-project

services:
  frontend:
    runtime: host
    cwd: ./frontend
    command: pnpm
    args: [dev]
    ports:
      - name: http
        value: auto
        preferred: 3000
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/

  backend:
    runtime: host
    cwd: ./backend
    command: uv
    args: [run, uvicorn, app.main:app, --port, "${PORT}"]
    env_file: [.env]
    ports:
      - name: http
        value: auto
        env: PORT
    depends_on: [redis]
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health

  redis:
    runtime: docker-compose
    compose:
      file: docker-compose.yml
      service: redis

startup:
  default: [frontend, backend]
```

The schema has exactly one canonical spelling for every concept. In particular
`ports:` is always a list (there is no singular `port:` form) and `command:` is
always a string (platform differences go in `platform.<os>.command`).

Point your editor at the JSON schema for completion:

```yaml
# yaml-language-server: $schema=https://devman.dev/schemas/devman.schema.json
```

## Requirements

- Go 1.27+
- Node 20+ and pnpm (desktop GUI)
- Rust stable (Tauri 2 desktop shell)

## Building

```bash
go build ./cmd/devman              # the CLI and daemon
go test ./...                      # unit and acceptance suites
go run ./tools/build dist          # every platform, archived, with SHA256SUMS
```

`tools/build` is Go rather than a Makefile so the same code produces a release
on a laptop and in CI. It stamps the version from `git describe`, or from
`DEVMAN_VERSION` when it is set, and `devman version` reports it back.

The desktop app bundles its own copy of the CLI: `go run ./tools/build sidecar`
runs automatically before every `pnpm tauri dev` and `pnpm tauri build`, and the
bundled binary lands next to the installed application — which is where the
window looks for it before falling back to `PATH`. A GUI launched from the Start
menu or the Dock often has a `PATH` that never saw your shell profile.

```bash
cd apps/desktop
corepack pnpm install
corepack pnpm tauri build          # NSIS installer on Windows
```

Releases are cut by pushing a tag: `git tag v0.1.0 && git push origin v0.1.0`.

Two of the acceptance fixtures run a real Python service. They skip themselves
when no interpreter is available; `DEVMAN_TEST_PYTHON` points them at a specific
one, which is how the FastAPI fixture is run against a virtual environment.


## Data directory

DevMan follows OS conventions for its own state:

- Windows: `%LOCALAPPDATA%\DevMan`
- macOS: `~/Library/Application Support/DevMan`
- Linux: `$XDG_STATE_HOME/devman`, else `~/.local/state/devman`

`DEVMAN_HOME` overrides it, and `devman paths` prints the resolved locations.
