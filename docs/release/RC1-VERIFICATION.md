# DevMan v0.1.0-rc.1 — release verification

This document records what was actually verified, on which machine, with which
versions. It is not a checklist of intentions: a gate that has not run on the
platform it claims to cover is written down as NOT VERIFIED, because "全部通过"
with no runner behind it is worse than an honest gap.

- Commit under verification: `b1fc5f023ae2d0e4e928acdd8510738fdbd412a1`
- Tag: none yet (`git tag` is empty; `v0.1.0-rc.1` is not cut)
- Remote: none configured (`git remote -v` is empty) — see Blockers
- Last updated: 2026-08-28

## Release gate status

- Gate 1 — `go test -count=1 ./...` on three platforms: PARTIAL (Windows only)
- Gate 2 — process tree fully terminated on three platforms: PARTIAL (Windows only)
- Gate 3 — port race, 20 concurrent, no duplicates, on three platforms: PARTIAL (Windows only)
- Gate 4 — daemon crash recovery on three platforms: PARTIAL (Windows only)
- Gate 5 — Docker Compose suite on Linux CI: NOT VERIFIED (passes locally on Windows + Docker Desktop)
- Gate 6 — Windows installer in a clean environment: NOT VERIFIED
- Gate 7 — release workflow, tag to artifacts: NOT VERIFIED

No gate is marked PASS. Four are blocked on the same missing resource.

## Local verification run

Machine: Windows 11, build 10.0.22631, x86_64. Toolchain: go1.27.0
windows/amd64. Docker engine 28.3.2, Docker Compose v2.38.2-desktop.1, Linux
containers.

### Unit and acceptance suites

`go test -count=1 ./...` — all packages ok. Notable results:

- `internal/acceptance` ok. `TestPythonService` PASS against the system CPython;
  `TestFastAPIService` SKIP because uvicorn is not installed on this machine. CI
  installs fastapi and uvicorn, so the skip must not appear in the CI log; if it
  does, the CI evidence is void.
- `internal/supervisor`, `internal/portmgr`, `internal/storage`,
  `internal/daemon`, `internal/logstore`, `internal/platform`, `internal/cli`,
  `internal/registry`, `internal/settings`, `internal/events`,
  `internal/envresolve`, `pkg/config` — ok.

### Host runtime gates — `tests/integration/host` (tag `integration`)

All three PASS on Windows:

- `TestStopTerminatesTheWholeProcessTree` — a three-level tree (service, child,
  grandchild) is fully gone after `devman stop`, the reported pid is the process
  DevMan started, the port becomes bindable again, and no allocation remains.
- `TestConcurrentStartsNeverShareAPort` — 20 projects all preferring 3000, all
  started concurrently from separate CLI instances: 20 distinct ports, each
  BOUND, each reported to exactly one caller, nothing left reserved after stop.
- `TestDaemonDeathLeavesAnHonestState` — the daemon is closed without stopping
  its services. On this machine the service survived (`recovery_test.go` logs
  `pid ... survived: true`), so the adoption branch ran: RUNNING, same pid,
  `adopted: true`, `log_capture: detached`, and a restart replaced the process
  and reattached capture.

Caveat worth keeping: inside one test process the Windows job object handle
outlives the stack, so the survivor branch is what runs locally. A daemon killed
from outside on Windows takes the *vanished* branch (job objects are created with
KILL_ON_JOB_CLOSE), which the same test asserts — CRASHED rather than STOPPED,
pid cleared, ports released, recovery ends with a working attached service. Both
branches are real assertions; neither is a skip. The Linux and macOS runs are
what will exercise the survivor branch under a genuinely dead daemon.

### Compose runtime gates — `tests/integration/compose` (tag `integration`)

Eight tests, all PASS locally. Every assertion is made against Docker
(`docker compose ps --format json`, `docker inspect`) rather than against
DevMan's own SQLite:

- `TestComposeLifecycle` — compose reports `running`; DevMan reports RUNNING;
  reserved port == published host port; port BOUND; URL present; HEALTHY;
  container output captured. After `devman stop`: STOPPED, container state
  `exited` and still present, follower process dead, zero allocations. Start
  again yields exactly one container; restart works.
- `TestComposeWaitsForDependencyHealth` — `condition: service_healthy` waits for
  the dependency's own healthcheck; the app's StartedAt trails the dependency's
  by at least 4s.
- `TestComposeRunningIsNotHealthy` — a container that is up while its endpoint
  returns 503 is RUNNING and UNHEALTHY. Runtime status and health status stay
  separate facts.
- `TestComposePortIgnoredByDockerIsUnverified` — with `"0:80"` Docker picks the
  host port, so the reservation no longer describes reality: UNVERIFIED, never
  BOUND, and the service still runs.
- `TestComposeStopPreservesVolume` — a named volume plus a startup counter shows
  `state 1`, then `state 2` after stop/start, proving stop is `compose stop` and
  not `down`.
- `TestMissingDockerIsBlocked` — docker absent from PATH: BLOCKED,
  `DOCKER_NOT_FOUND`.
- `TestDockerDaemonUnavailableIsBlocked` — `DOCKER_HOST` at a closed port:
  BLOCKED, `DOCKER_UNAVAILABLE` (never `COMMAND_NOT_FOUND`).
- `TestUnknownComposeServiceIsRefusedAtRegister` — a misspelled
  `compose.service` fails at register with `CONFIG_INVALID`.
- `TestComposeContainerCrashIsReported` — a container exiting 7 with
  `restart.policy: "no"` is reported CRASHED and its ports are released.

The fixture image is built offline: a static Go binary in a `FROM scratch`
image, so a failure means DevMan is broken rather than that a registry was slow.
Each test uses its own compose project (`devman-it-<name>-<pid>`) and is torn
down with `down -v --remove-orphans`.

This suite has never run on Linux. Until it does, Gate 5 is NOT VERIFIED —
`DEVMAN_REQUIRE_DOCKER=1` in the `integration-compose` job exists precisely so a
skip there fails the build instead of looking green.

### Packaging

`go run ./tools/build dist` produced six archives plus `SHA256SUMS`:
windows/amd64, windows/arm64, darwin/amd64, darwin/arm64, linux/amd64,
linux/arm64. Each archive contains the binary, `README.md` and `UNSIGNED.txt`
(verified by listing the zip). macOS is covered for both Intel and Apple
Silicon; the artifacts are unsigned, which is a missing certificate, not a
missing platform.

The Tauri sidecar (`go run ./tools/build sidecar`) names the binary from
`rustc -vV`'s host triple, so it is correct on whichever machine builds the
bundle. Only the Windows installer is produced today; macOS and Linux bundles
need a machine of that OS in the release workflow, which is a v0.1 follow-up
rather than an RC blocker.

## Defects found and fixed during hardening

All three were found by tests that ask the operating system or Docker rather
than DevMan:

1. P0 — the compose log follower used `logs -f --tail 0`, discarding everything
   a container printed before DevMan attached, including the reason an image
   fails to boot. Fixed to `--tail all` (`3119900`).
2. P0 — every `docker compose up` failure was reported as an internal error, so
   "Docker Desktop isn't running", "your service name is a typo" and "DevMan is
   broken" were indistinguishable and all landed in FAILED. Added
   `DOCKER_UNAVAILABLE` (BLOCKED, 422) via a `docker info` probe and
   `CONFIG_INVALID` for an unknown service (`3119900`).
3. P1 — `compose.service` was never checked against the compose file, and
   validation resolved a relative `compose.file` against the project root while
   the runtime resolves it against the service cwd, so validation could pass on
   a file the runtime never opens. Both fixed (`3119900`).

## Open items, classified

- P0 — three-platform CI evidence for Gates 1–4. Code is in place
  (`.github/workflows/ci.yml`, jobs `unit` and `integration-host` on
  windows/ubuntu/macos-latest); no run exists because there is no remote.
- P0 — Gate 5 on Linux CI (`integration-compose`, `DEVMAN_REQUIRE_DOCKER=1`).
- P0 — Gate 6: Windows installer smoke test on a clean machine with no Go, Node,
  pnpm, Rust, Python or Git: install, Start-menu entry, GUI starts, bundled
  daemon starts, discovery file and auth token written, SQLite created, register
  a tiny fixture exe, start/health/logs/stop, then uninstall removing app,
  binaries and shortcuts while leaving user data intact.
- P0 — Gate 7: tag `v0.1.0-rc.1`, release workflow runs, artifacts attached.
- P1 — upgrade smoke test: old build installed, new installer over it, database,
  config and registry preserved.
- P1 — `DEVMAN_UI_TEST=1` entry point so GUI screenshots do not depend on an
  unlocked screen, plus visual QA of the nine pages in Chinese and English at
  100/125/150% DPI.
- P2 — macOS and Linux desktop bundles, and signing identities for all
  platforms.

## Blockers

No remote repository. `git remote -v` is empty and the `gh` CLI is not installed
on this machine, so the 6 hardening commits (`30f34fb`, `3119900`, `0d24af1`,
`bf721ff`, `5010392`, `b1fc5f0`) on top of the earlier history cannot be pushed,
CI has never executed on any runner, and the release workflow cannot be
exercised. Four of the seven gates depend on this single step.

Nothing here can be signed off as ACCEPTED until those runs exist and their
runner names, versions and results replace the PARTIAL and NOT VERIFIED lines
above.
