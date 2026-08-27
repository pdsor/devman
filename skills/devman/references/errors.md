# Error codes

Every failure carries a stable `code`. Branch on it; never on the message.

```json
{ "error": { "code": "PORT_CONFLICT", "message": "8000 is in use", "path": "services.api.ports[0]", "details": { "pid": 4312, "process": "python.exe" } } }
```

`path` points at the offending field in `devman.yaml` when there is one, and
`details` carries whatever the daemon could resolve (holding PID, process name,
attempt counts).

## Configuration

| Code | Meaning | What to do |
| --- | --- | --- |
| `CONFIG_NOT_FOUND` | no `devman.yaml` | `devman init`, then edit it |
| `CONFIG_INVALID` | schema or semantic error | read `path`; the usual causes are a `command` mapping, `port:` instead of `ports:`, `shell: true` with `args`, or an unknown field |
| `COMMAND_NOT_FOUND` | `command` is not on PATH and is not an existing file | check the executable name, add the `.cmd` suffix on Windows, or point at the venv interpreter |

## Registration and trust

| Code | Meaning | What to do |
| --- | --- | --- |
| `PROJECT_NOT_FOUND` | directory is not registered | `devman register .` |
| `PROJECT_EXISTS` | already registered, or `devman.yaml` exists and `--force` was not passed | nothing, usually |
| `PROJECT_UNTRUSTED` | the executable parts of the config changed, or approval was never given | show the user the commands, then `devman trust` |
| `SERVICE_NOT_FOUND` | no such service in this project | check `devman status --json` for the real names |

## Ports

| Code | Meaning | What to do |
| --- | --- | --- |
| `PORT_CONFLICT` | a fixed port is taken; `details` names the holder where the OS allows | free it, or switch to `value: auto` with `preferred` |
| `PORT_EXHAUSTED` | no free port in the named range | widen the range with `devman config set port_ranges.<name>.end`, or stop something |

## Runtime

| Code | Meaning | What to do |
| --- | --- | --- |
| `SERVICE_BLOCKED` | a prerequisite is unmet; nothing was launched | fix the prerequisite named by the service's `reason.code` |
| `ENV_MISSING` | a `required_env` variable is unset | create `.env`, or export it; never invent a value |
| `DOCKER_NOT_FOUND` | `docker` is not available | start Docker Desktop, or drop the compose service |
| `DEPENDENCY_FAILED` | a `depends_on` service did not come up | fix that service first; the dependent one was deliberately not launched into a broken environment |
| `HEALTHCHECK_FAILED` | the probe never passed | check the URL and the service's own logs; the probe may be pointing at a route that does not exist |
| `PROCESS_CRASHED` | it was running and exited | `devman logs <service> --tail 200` and `last_exit_code` |
| `ALREADY_RUNNING` | start requested for a running service | use `restart` if a fresh process is wanted |
| `NOT_RUNNING` | stop requested for something already stopped | nothing |
| `TIMEOUT` | an operation exceeded its budget | usually a slow first start; retry with a longer `--wait` |

## Daemon and transport

| Code | Meaning | What to do |
| --- | --- | --- |
| `DAEMON_NOT_RUNNING` | nothing is listening | retry — most commands auto-start the daemon; `devman daemon start` does it explicitly |
| `UNAUTHORIZED` | bad or missing token, or a rejected Origin | do not work around it; re-read the token from the path in `devman paths` |
| `INVALID_REQUEST` | malformed call or unknown flag | fix the invocation |
| `UNSUPPORTED` | not available on this platform | report it; do not emulate it by hand |
| `INTERNAL` | a bug in DevMan | capture the message and the surrounding logs |

## Status values, for comparison

A `status` is not an error. `BLOCKED` and `FAILED` are the two that carry a
`reason` with one of the codes above:

- `BLOCKED` — prerequisite missing, nothing attempted, nothing broken
- `FAILED` — the start was attempted and failed
- `CRASHED` — was running, exited on its own
- `RUNNING` + `health.status: UNHEALTHY` — alive, not ready
- `RUNNING` + `ports[].status: UNVERIFIED` — alive, never bound the port it was
  given, so the injected variable is probably not wired up
