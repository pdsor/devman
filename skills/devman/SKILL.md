---
name: devman-project-manager
description: Use when a project's dev services (frontend, backend, worker, database) need to be started, stopped, inspected or configured — including "start the dev server", "把这个项目加入 DevMan", "why is the backend not up", "which port is the frontend on", "show me the backend logs", or generating and validating a devman.yaml. DevMan owns long-running dev processes so the agent's shell never has to; use this skill instead of running `npm run dev`, `uvicorn`, `docker compose up` or any other long-lived command in a terminal.
---

# DevMan project manager

DevMan is a local daemon that owns the lifecycle of a project's development
services. This skill is how an agent uses it.

## The rule that matters

**Never run a long-lived dev command in your own shell.** Not `npm run dev`, not
`uvicorn`, not `docker compose up`, not `go run .`, not with `&`, `nohup` or a
background flag. Those processes die with the session, leak ports, and leave the
user with no way to see or stop them.

Hand them to DevMan instead:

```bash
devman start                    # starts the project's services, in dependency order
devman status                   # what is running, healthy, on which port
devman logs backend --tail 100  # output, without owning the process
devman stop                     # stops them, and remembers they should stay stopped
```

Short-lived commands (`npm install`, `pytest`, `alembic upgrade head`, `git`) are
still yours to run directly. DevMan is for processes that are supposed to keep
running.

## Every command speaks JSON

Add `--json` to anything and branch on the result instead of parsing tables:

```bash
devman status --json
devman start --json --wait 30s
devman validate --json
devman ports --json
```

Errors are JSON too, with a stable `code`:

```json
{ "error": { "code": "PROJECT_UNTRUSTED", "message": "...", "path": "services.web.command" } }
```

Branch on `error.code`, never on the message text. The codes you will actually
meet are in `references/errors.md`.

## Deciding what to do

Run this first, in the project directory:

```bash
devman status --json
```

- **Works, services listed** → the project is registered. Use `start`, `stop`,
  `logs`, `ports`. You are done reading this file.
- `DAEMON_NOT_RUNNING` → no daemon; any command other than `daemon status` will
  start one for you, so just retry. If `devman` itself is not on PATH, DevMan is
  not installed: tell the user, do not fall back to running services yourself.
- `PROJECT_NOT_FOUND` → not registered yet. Continue below.
- `CONFIG_NOT_FOUND` → no `devman.yaml`. Continue below.

## Onboarding a project

1. **Read the repository before writing anything.** Detection rules and the
   file-by-file evidence to use are in `references/detection-rules.md`. Use the
   commands the project already documents (`package.json` scripts, a Makefile
   target, the README) — do not invent a start command.
2. **Write `devman.yaml`** at the project root. `devman init` scaffolds one;
   editing that scaffold is usually faster than writing from scratch. The full
   field reference is `references/schema.md`, worked examples for Node, Python,
   Go, monorepos and Docker are in `references/examples.md`.
3. **Validate before registering:**
   ```bash
   devman validate --json
   ```
   Fix every entry in `errors`. Warnings are advisory — read them, then decide.
4. **Show the user what will run, and let them approve it:**
   ```bash
   devman register .
   ```
   This prints the commands, working directories, env files and Docker targets
   the project will be allowed to execute, and waits for a yes. That prompt is a
   security boundary, so:
   - In an interactive session, let the **user** answer it.
   - Non-interactively, `devman register . --trust` records approval on the
     user's behalf. Only pass `--trust` when the user has actually seen and
     approved the commands — the whole point of the check is that a `devman.yaml`
     is executable code.
5. **Start and verify:**
   ```bash
   devman start --json --wait 30s
   ```
   Then report the URLs and ports from the result. Do not claim a service is up
   because the command exited 0; check `status` and `health.status` per service.

## Reading a failure

`status --json` gives you both the state and the reason. The distinction that
matters:

- `BLOCKED` — a prerequisite is missing (Docker not installed, `required_env`
  unset, a fixed port taken). Nothing is broken and nothing was launched: fix the
  prerequisite. `reason.code` says which.
- `FAILED` — the start was attempted and failed.
- `CRASHED` — it was running and exited on its own. `last_exit_code` and
  `devman logs <service> --tail 200` are where the answer is.
- `RUNNING` + `health.status: UNHEALTHY` — the process is alive but its probe
  fails. That is a bug in the service, not in DevMan.
- `ports[].status: UNVERIFIED` — DevMan allocated the port and the service never
  bound it. Usually the service ignores `$PORT`; wire the injected variable in.

## Command reference

```bash
devman init [path]                 # scaffold a devman.yaml
devman validate [path]             # check config; no daemon needed
devman register . [--trust]        # approve and register
devman trust [--revoke]            # re-approve after editing commands
devman list                        # registered projects
devman status [--all]              # services, health, ports, URLs
devman start [services...] [--wait 30s] [--profile name] [--all]
devman stop  [services...]
devman restart [services...]       # re-reads devman.yaml
devman logs <service> [--tail N] [--stream stderr] [--since RFC3339] [--follow]
devman ports [port]                # allocations, or who holds one port
devman open [service]              # open a service's URL
devman events [--limit N] [--follow]
devman config list|get|set         # global settings
devman paths                       # where DevMan keeps its data
devman daemon start|stop|status|restart
```

Flags work after positional arguments, and `--project <id|name|path>` targets a
project other than the current directory. `--follow` streams until interrupted —
do not use it in a non-interactive tool call, use `--tail` instead.

## After a config edit

Editing `command`, `args`, `cwd`, `shell`, `env`, `env_file`, `runtime` or a
compose target changes what the project may execute, so trust is withdrawn and
starting fails with `PROJECT_UNTRUSTED`. Show the user the diff, then:

```bash
devman validate --json && devman trust   # or: devman trust --yes, once approved
devman restart --json
```

Cosmetic edits (`display_name`, a health interval) keep trust. `restart` re-reads
the file; there is no reload command.

## Things not to do

- Do not edit `devman.yaml` to work around a port conflict. `value: auto` with a
  `preferred` port lets two projects both want 3000 and both start.
- Do not add a health probe the project cannot serve. No `health:` block means
  process-level health, reported as `N/A`, which is honest. A fabricated
  `/health` URL turns a working service into a permanently UNHEALTHY one.
- Do not use `${ENV:PORT}` — it is rejected. DevMan-allocated ports are only
  reachable through `${PORT}` / `${PORT:name}`.
- Do not put per-OS differences in `command` as a mapping; use
  `platform.windows.command`. On Windows, `npm`/`pnpm`/`yarn` need the `.cmd`
  suffix.
- Do not kill DevMan-managed PIDs by hand. `devman stop` takes down the whole
  process tree and releases the ports; a manual kill leaves reservations behind.

## References

- `references/schema.md` — every `devman.yaml` field, with the rules the
  validator enforces
- `references/detection-rules.md` — how to infer services, commands, cwd, ports
  and dependencies from a repository
- `references/examples.md` — complete configurations for Node, Python, Go,
  monorepo and Docker Compose projects
- `references/errors.md` — every error code and the action it implies
- `scripts/devman-check.ps1` / `scripts/devman-check.sh` — one call that reports
  DevMan availability, config presence, validity and registration as JSON

The check script deliberately starts nothing, so it is safe to run first. Its
`next_action` is one of:

| `next_action` | Meaning |
| --- | --- |
| `install_devman` | `devman` is not on PATH; tell the user, do not run services yourself |
| `init` | no `devman.yaml`; onboard the project |
| `fix_config` | config invalid; `config_output` holds the validator's own message |
| `query_status` | config is fine but the daemon is not running; `devman status --json` will start it and answer the rest |
| `register` | valid config, not registered; `devman register .` |
| `trust` | registered but the commands changed; `devman trust` after the user reviews them |
| `start` | ready and stopped; `devman start --wait 30s` |
| `nothing` | already running |
