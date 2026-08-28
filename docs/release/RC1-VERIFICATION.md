# DevMan v0.1.0-rc.1 — release verification

This document records what was actually verified, on which machine, with which
versions. It is not a checklist of intentions: a gate that has not run on the
platform it claims to cover is written down as NOT VERIFIED, because "全部通过"
with no runner behind it is worse than an honest gap.

- Commit under verification: `4c17faf` (CI run 33159386171, all ten jobs green)
- Tag: none yet (`git tag` is empty; `v0.1.0-rc.1` is not cut)
- Remote: `git@github.com:pdsor/devman.git`, branch `main`
- Last updated: 2026-08-28



## Release gate status

- Gate 1 — `go test -count=1 ./...` on three platforms: PASS — windows-latest,
  ubuntu-latest and macos-latest all green
- Gate 2 — process tree fully terminated on three platforms: PASS —
  `integration-host` green on windows-latest, ubuntu-latest, macos-latest
- Gate 3 — port race, 20 concurrent, no duplicates, on three platforms: PASS —
  same three runners, after the fix in `a2d2450`
- Gate 4 — daemon crash recovery on three platforms: PASS — same three runners
- Gate 5 — Docker Compose suite on Linux CI: PASS — `integration-compose` green
  with `DEVMAN_REQUIRE_DOCKER=1`, so a skip would have failed the job
- Gate 6 — Windows installer in a clean environment: NOT VERIFIED
- Gate 7 — release workflow, tag to artifacts: NOT VERIFIED

Five of seven gates have three-platform runner evidence. Gates 6 and 7 have
never run, and neither can be signed off from this machine.

### Three-platform CI evidence

Repository `pdsor/devman`, workflow `CI`, run 33159386171 on `4c17faf`. Job
results read back with `gh run view --json jobs`:


```
success  lint
success  unit / ubuntu-latest
success  unit / windows-latest
success  unit / macos-latest
success  integration-host / ubuntu-latest
success  integration-host / windows-latest
success  integration-host / macos-latest
success  integration-compose
success  desktop
success  package
```

Three earlier runs were red, and each transition is the evidence for one fix
rather than a rerun: `integration-host / windows-latest` went green with
`a2d2450` (port probe), `unit / macos-latest`'s identity test with `882bba1`
(darwin executable path), and its python test with `39bfe4c` (the fixture's
reverse-DNS hang).


### Windows end-to-end run, by hand

Not a test binary: the real CLI, a project that did not exist before, both
services written as ordinary apps.

Machine: Windows 11 build 10.0.22631 x86_64, go1.27.0, Python 3.12.10,
Node v24.19.0. Binary: `go build ./cmd/devman`, reported version `0.1.0-dev`.

1. `devman-check.ps1` in an empty project → `next_action: init`,
   `daemon_running: false`. Nothing was started by the check.
2. `devman init .` scaffolded `devman.yaml`; it was replaced with two host
   services — a Python API (`python server.py`, `preferred: 8000`, range
   `backend`, http health probe) and a Node web app (`npm run dev` with
   `platform.windows.command: npm.cmd`, `preferred: 3000`, range `frontend`,
   `depends_on: api condition healthy`).
3. `devman validate --json` rejected the first draft:
   `CONFIG_INVALID … ${PORT:api.http} refers to an undeclared port name` at
   `services.web.env.API_URL`. Cross-service port interpolation is not a v0.1
   feature; the reference was removed and validation returned
   `{"valid": true, "errors": [], "warnings": []}`.
4. `devman register . --trust` → `trusted: true`, project id `p_fc61fb70dca9`.
5. `devman start --json --wait 40s` → `api` RUNNING/HEALTHY on 8000 (BOUND),
   `web` RUNNING on 3000. `web` has no health block, so health is `N/A`, which
   is the honest answer rather than a fabricated probe.
6. Verified against the OS, not against DevMan:
   `GET http://127.0.0.1:8000/health` → `{"status": "ok"}`,
   `GET http://127.0.0.1:3000/` → `{"service":"web","port":3000,...}`.
   `devman ports` listed 3000 and 8000 BOUND to the right service.
7. Port management under contention: a second project with the same preferred
   ports was registered and started. It received 8001 and 3001, both BOUND,
   both answering; the first project kept 8000 and 3000. No conflict was
   reported to either caller and nothing was reassigned.
8. `devman logs --project … web --tail 5` showed the npm banner and
   `web listening on 3001`, so capture follows through `npm.cmd` into `node`.
9. `devman stop` on both projects → `no ports allocated`,
   `Get-NetTCPConnection` finds nothing listening on 3000, 3001, 8000 or 8001,
   and status reports STOPPED. The `npm.cmd` → `node` tree left no survivor.

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
4. P0 — `docker info` exits 0 on Docker CE 28.0.4 with an unreachable daemon
   (Docker Desktop 28.3.2 exits non-zero), so the engine probe reported INTERNAL
   instead of `DOCKER_UNAVAILABLE` on the Linux runner. Replaced with
   `docker version --format {{.Server.Version}}`, a question only a server can
   answer (`ff97b0f`).
5. P1 — on macOS, `Inspect` passed the `kern.procargs2` result to `os.Readlink`,
   so the executable was never recorded and the identity check that is supposed
   to reject a recycled PID had nothing to compare. The platform hook now
   returns a resolved path (`882bba1`).
6. P0 — bind verification asked the OS "could I bind this port?" every 100ms for
   up to fifteen seconds, on the very port a starting service had been told to
   listen on. On Windows that probe is exclusive, so a child whose bind landed
   inside a probe window died with "Only one usage of each socket address" — the
   CRASHED racer in `integration-host / windows-latest`. Verification now asks by
   connecting, and reservation claims the database row before probing, so DevMan
   never probes a port it has already handed out (`a2d2450`). Found by making the
   test print the crashed racer's captured stderr instead of only its DTO.
7. P1 — the Windows console keeps the system's legacy code page, so on a Chinese
   install every non-ASCII byte DevMan writes — a service's own log lines
   included — reached the terminal as mojibake. The CLI now switches the console
   to UTF-8 for the duration of a command and restores the previous code page
   (`0690b59`). Unverified; see Open items.

Two more were diagnosed but turned out to be in the test fixture rather than in
DevMan, and are recorded because the diagnosis is the useful part:

- On macos-latest the python fixture hung inside its own constructor:
  `HTTPServer.server_bind` calls `socket.getfqdn` on the bind address after
  `bind()` and before `listen()`, and that reverse lookup blocks on a hosted
  runner. The socket was bound but not listening, so connections timed out
  rather than being refused. DevMan reported RUNNING (the process was alive),
  the port UNVERIFIED (nothing had bound it) and health UNHEALTHY (nothing
  answered) — three correct facts about a service that really was deaf
  (`39bfe4c`).
- The Tauri desktop job failed because `tauri_build` resolves `externalBin` at
  compile time and the sidecar directory is gitignored, so only a machine that
  had built it before could compile. The job builds the sidecar now (`55b8728`).



## Changes after `39bfe4c`

Three commits landed after `39bfe4c` and are covered by the CI run above, which
was taken on `4c17faf`. Two are bug fixes; one adds resource reporting, which the
user asked for explicitly and which is therefore a scoped exception to the freeze
rather than a silent addition.


- `5ed2838` — three desktop defects. `Summarise` counted services nobody asked
  to run, so a project with an optional worker, or one service deliberately
  stopped, reported DEGRADED while sitting in the state that was requested. The
  project card had a redundant Open button next to Start and Stop. A service URL
  was a button calling the opener plugin, but `opener:allow-open-url` grants the
  command "without any pre-configured scope", so every call was denied and the
  `void` discarded the rejection: the button looked dead.
- `49a7ad2` — `internal/platform` gained the read-only half: cumulative CPU time
  and resident memory for a process tree, and host CPU and memory. No
  percentages, because a percentage describes an interval and belongs to
  whichever layer holds two samples.
- `de966b7` — a 1 Hz sampler in the supervisor, `GET /api/v1/machine/usage`,
  per-service and per-project usage in the API, and the desktop's sidebar meter,
  project total and per-service figure.

### Local evidence for these three commits

Run on the same Windows machine as the rest of this document, 2026-08-28:

- `go test ./...` — all packages ok, no failures.
- `go vet ./...` under `GOOS=windows`, `GOOS=linux` and `GOOS=darwin` — clean,
  which is what covers the two platform files that cannot run here.
- `gofmt -l internal pkg` — no output.
- `pnpm build` in `apps/desktop` (`tsc --noEmit && vite build`) — exit 0.
- `internal/supervisor/summarise_test.go` — 6 subtests. Optional worker stopped
  → HEALTHY; `N/A` health → HEALTHY; a wanted service that crashed → DEGRADED;
  an UNHEALTHY probe → DEGRADED; everything stopped → STOPPED; a service that
  crashed while stopping → HEALTHY.
- `internal/supervisor/usage_test.go` — the percentage arithmetic against
  counters chosen on purpose: 0.5s of CPU in a 4-core second is 12.5%; 512 MB of
  16 GiB is 3.125%; a machine 0.8s busy out of 4s of elapsed CPU time is 20%; a
  tree that loses a process mid-interval reports 0% rather than negative work; a
  stopped service is forgotten instead of freezing at its last reading.
- `internal/platform/usage_test.go` — measures the test binary itself after a
  busy loop, so a sampler returning nothing for a process that provably ran
  fails on whichever platform it is run on. Also walks a deliberately cyclic
  parent table, which two reads of a changing machine can produce.
- `internal/acceptance/usage_test.go` — a real service under a real daemon, read
  back over the API the desktop uses: usage appears with a plausible memory
  share, the project total is at least its only service's, the service cannot
  hold more memory than the machine reports in use, the reading disappears when
  the service is stopped, and a never-started service has none. 2.06s, PASS.

### What these three commits still owe

- The opener scope fix is reasoned from the generated

  `apps/desktop/src-tauri/gen/schemas/desktop-schema.json`, which documents the
  permission as scopeless and defines `OpenerScopeEntry` with a `url` field
  taking glob wildcards. No one has clicked a service URL in a rebuilt window
  yet, so the fix is argued, not observed.
- The sidebar meter, the project total and the per-service figure have not been
  seen rendered. The window open on this machine runs an older build.
- macOS host CPU is the sum of per-process CPU time, which is the only
  machine-wide counter reachable without CGO. It omits kernel time not billed to
  a process and can move backwards when processes exit; the sampler clamps a
  negative delta to zero. This is a stated limitation in the code, not a defect
  to fix under the freeze.


## Open items, classified


- P0 — Gate 6: Windows installer smoke test on a clean machine with no Go, Node,
  pnpm, Rust, Python or Git: install, Start-menu entry, GUI starts, bundled
  daemon starts, discovery file and auth token written, SQLite created, register
  a tiny fixture exe, start/health/logs/stop, then uninstall removing app,
  binaries and shortcuts while leaving user data intact.
- P0 — Gate 7: tag `v0.1.0-rc.1`, release workflow runs, artifacts attached.
- P1 — click a service URL in a rebuilt desktop window and confirm a browser

  opens, which is the only thing that turns the opener-scope fix in `5ed2838`
  from argued into observed.
- P1 — look at the sidebar CPU/memory meter, the project total and the
  per-service figure in a rebuilt window. The numbers are covered by tests; the
  rendering is not.

- P1 — upgrade smoke test: old build installed, new installer over it, database,
  config and registry preserved.
- P1 — visual QA of the nine GUI pages in Chinese and English at 100/125/150%
  DPI. The fixture daemon (`tools/uifixture`, `VITE_DEVMAN_UI_TEST`) exists so
  this no longer depends on an unlocked screen; the pass itself has not been
  done.
- P1 — the Windows console code page fix (`0690b59`) is unverified. It cannot be
  checked from an automated shell: every command run here has stdout redirected,
  and a redirected stdout is exactly the case the fix deliberately skips. It
  needs one human running `devman logs` on a CP936 console against a service
  that prints non-ASCII.
- P2 — cross-service port interpolation (`${PORT:<service>.<name>}`) is
  rejected by the validator. Out of scope under the freeze; worth a documented
  limitation so nobody writes it expecting it to work.
- P2 — macOS and Linux desktop bundles, and signing identities for all
  platforms.


## Blockers

None outstanding. The remote exists, CI is green on all three platforms for
`39bfe4c`, and the compose suite runs on Linux with `DEVMAN_REQUIRE_DOCKER=1`.
What is left is work that has not been done yet rather than work that cannot be
done: a CI re-run on the current head, an installer to exercise on a clean
machine, a GUI pass, and a tag to cut.

Gates 6 and 7 cannot be signed off from this machine's evidence, so
`v0.1.0-rc.1` is not cut.



