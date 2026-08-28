package main

import (
	"path/filepath"
	"time"

	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// The canned world.
//
// The states here are chosen for what they stress in the interface rather than
// for looking tidy: a service that is RUNNING and UNHEALTHY at once, a CRASHED
// one with an exit code, a BLOCKED one whose precondition is outside DevMan, a
// port in CONFLICT, an adopted service with detached log capture, a display name
// long enough in CJK to overflow a column, and a path deep enough to be
// truncated. Those are the cells that break a layout, and they are exactly the
// ones that never appear in a demo project.

func newWorld(scenario string, layout paths.Layout, port int) *world {
	w := &world{
		layout:  layout,
		port:    port,
		started: time.Now().Add(-73 * time.Minute).UTC(),
		settings: map[string]string{
			"daemon.port_start":         "39100",
			"daemon.port_end":           "39149",
			"port_ranges.frontend":      "3000-3999",
			"port_ranges.backend":       "8000-8999",
			"port_ranges.general":       "10000-19999",
			"defaults.graceful_timeout": "10s",
			"defaults.health_interval":  "5s",
			"defaults.start_timeout":    "60s",
			"logs.max_size_mb":          "10",
			"logs.max_backups":          "5",
		},
		logs:   sampleLogs(),
		events: sampleEvents(),
	}

	if scenario == "empty" {
		// The first-run state is a real screen with its own layout problems, and
		// it is the one every new user sees first.
		w.projects = nil
		w.ports = nil
		w.events = w.events[:1]
		w.logs = nil
		return w
	}

	w.projects = []dto.Project{shopProject(), platformProject(), staleProject()}
	for i := range w.projects {
		resummarise(&w.projects[i])
	}
	w.ports = samplePorts()
	return w
}

func at(offset time.Duration) *time.Time {
	moment := time.Now().Add(offset).UTC()
	return &moment
}

func exit(code int) *int { return &code }

func shopProject() dto.Project {
	root := `C:\work\shop`
	return dto.Project{
		ID:            "prj_shop",
		Name:          "shop",
		DisplayName:   "商城前台（含结算与库存联调环境）",
		Path:          root,
		ConfigPath:    filepath.Join(root, "devman.yaml"),
		Favorite:      true,
		Trusted:       true,
		CreatedAt:     time.Now().Add(-32 * 24 * time.Hour).UTC(),
		UpdatedAt:     time.Now().Add(-2 * time.Hour).UTC(),
		LastStartedAt: at(-71 * time.Minute),
		Services: []dto.Service{
			{
				Project: "prj_shop", Name: "web", DisplayName: "Web", Runtime: "host",
				Status: dto.StatusRunning, DesiredState: dto.DesiredRunning,
				Health: dto.HealthResult{
					Status: dto.HealthHealthy, Type: "http",
					Target: "http://127.0.0.1:3000/", CheckedAt: at(-3 * time.Second), LatencyMS: 12,
				},
				PID: 24188, StartedAt: at(-71 * time.Minute), UptimeSeconds: 4260,
				CommandLine: "pnpm dev --port 3000", CWD: root,
				URL: "http://127.0.0.1:3000",
				Ports: []dto.PortAllocation{{
					Port: 3000, Name: "http", Project: "shop", Service: "web", EnvVar: "PORT",
					Status: dto.PortBound, AllocatedAt: time.Now().Add(-71 * time.Minute).UTC(),
				}},
				Observability: dto.Observability{LogCapture: dto.LogCaptureAttached},
			},
			{
				// RUNNING and UNHEALTHY together. If the interface ever collapses
				// these two into one badge, this row is what makes it obvious.
				Project: "prj_shop", Name: "api", DisplayName: "API", Runtime: "host",
				Status: dto.StatusRunning, DesiredState: dto.DesiredRunning,
				Health: dto.HealthResult{
					Status: dto.HealthUnhealthy, Type: "http",
					Target: "http://127.0.0.1:8012/health", CheckedAt: at(-2 * time.Second),
					LatencyMS: 3004, Failures: 6,
					Message: "context deadline exceeded after 3s",
				},
				PID: 24512, StartedAt: at(-70 * time.Minute), UptimeSeconds: 4200,
				RestartCount: 2,
				CommandLine:  "python -m uvicorn app:app --host 127.0.0.1 --port 8012",
				CWD:          filepath.Join(root, "api"),
				URL:          "http://127.0.0.1:8012",
				DependsOn:    []string{"redis"},
				Ports: []dto.PortAllocation{{
					Port: 8012, Name: "http", Project: "shop", Service: "api", EnvVar: "PORT",
					Status: dto.PortBound, AllocatedAt: time.Now().Add(-70 * time.Minute).UTC(),
				}},
				Observability: dto.Observability{LogCapture: dto.LogCaptureAttached},
			},
			{
				// Process-only health: N/A is a legitimate answer, not a gap.
				Project: "prj_shop", Name: "redis", DisplayName: "Redis", Runtime: "docker-compose",
				Status: dto.StatusRunning, DesiredState: dto.DesiredRunning,
				Health: dto.HealthResult{Status: dto.HealthNotApplicable, Type: "process"},
				PID:    24020, StartedAt: at(-72 * time.Minute), UptimeSeconds: 4320,
				CommandLine: "docker compose -f docker-compose.yml logs -f --tail all redis",
				CWD:         root,
				Ports: []dto.PortAllocation{{
					Port: 6379, Name: "redis", Project: "shop", Service: "redis",
					Status: dto.PortBound, AllocatedAt: time.Now().Add(-72 * time.Minute).UTC(),
				}},
				Observability: dto.Observability{LogCapture: dto.LogCaptureAttached},
			},
		},
	}
}

func platformProject() dto.Project {
	root := `D:\workspace\customers\acme-corporation\platform\services\ingest-pipeline`
	return dto.Project{
		ID:            "prj_ingest",
		Name:          "ingest-pipeline",
		Path:          root,
		ConfigPath:    filepath.Join(root, "devman.yaml"),
		Trusted:       true,
		CreatedAt:     time.Now().Add(-9 * 24 * time.Hour).UTC(),
		UpdatedAt:     time.Now().Add(-11 * time.Minute).UTC(),
		LastStartedAt: at(-26 * time.Minute),
		Services: []dto.Service{
			{
				// CRASHED, not STOPPED: nobody asked for this, and the exit code
				// is the first thing a user looks for.
				Project: "prj_ingest", Name: "worker", Runtime: "host",
				Status: dto.StatusCrashed, DesiredState: dto.DesiredRunning,
				Health:       dto.HealthResult{Status: dto.HealthUnknown, Type: "process"},
				LastExitCode: exit(7), RestartCount: 3,
				CommandLine: "node dist/worker.js",
				CWD:         root,
				Message:     "worker exited unexpectedly with code 7",
				Reason: (&dto.Error{
					Code: string(errs.CodeProcessCrashed), Message: "worker kept exiting; gave up after 3 attempts",
				}),
				Observability: dto.Observability{LogCapture: dto.LogCaptureNone},
			},
			{
				// BLOCKED is not FAILED: nothing was attempted, and the fix is
				// outside DevMan.
				Project: "prj_ingest", Name: "db", Runtime: "docker-compose",
				Status: dto.StatusBlocked, DesiredState: dto.DesiredRunning,
				Health:  dto.HealthResult{Status: dto.HealthUnknown, Type: "http"},
				CWD:     root,
				Message: "the Docker engine is not reachable; start Docker Desktop and try again",
				Reason: (&dto.Error{
					Code:    string(errs.CodeDockerUnavailable),
					Message: "the Docker engine is not reachable",
					Path:    "services.db.compose",
				}),
				Observability: dto.Observability{LogCapture: dto.LogCaptureNone},
			},
			{
				// Adopted with detached capture: the process outlived a daemon, so
				// `logs` has nothing live to show until it is restarted. The
				// interface has to say that rather than showing an empty pane.
				Project: "prj_ingest", Name: "gateway", Runtime: "host",
				Status: dto.StatusRunning, DesiredState: dto.DesiredRunning,
				Health: dto.HealthResult{
					Status: dto.HealthHealthy, Type: "tcp",
					Target: "127.0.0.1:8431", CheckedAt: at(-4 * time.Second), LatencyMS: 2,
				},
				PID: 9032, StartedAt: at(-5 * time.Hour), UptimeSeconds: 18000,
				CommandLine: "go run ./cmd/gateway",
				CWD:         root,
				URL:         "http://127.0.0.1:8431",
				Ports: []dto.PortAllocation{{
					Port: 8431, Name: "http", Project: "ingest-pipeline", Service: "gateway",
					EnvVar: "PORT", Status: dto.PortBound,
					AllocatedAt: time.Now().Add(-5 * time.Hour).UTC(),
				}},
				Observability: dto.Observability{LogCapture: dto.LogCaptureDetached, Adopted: true},
			},
		},
	}
}

// staleProject is registered but currently unusable: its config no longer parses
// and its fingerprint was never approved. Both refusals have to be legible from
// the project list, not only after clicking start.
func staleProject() dto.Project {
	root := `C:\work\legacy-admin`
	return dto.Project{
		ID:         "prj_legacy",
		Name:       "legacy-admin",
		Path:       root,
		ConfigPath: filepath.Join(root, "devman.yaml"),
		Trusted:    false,
		ConfigError: (&dto.Error{
			Code:    string(errs.CodeConfigInvalid),
			Message: "services.admin.ports[0].value must be a number, \"auto\", or \"${VAR}\"",
			Path:    "services.admin.ports[0].value",
		}),
		CreatedAt: time.Now().Add(-140 * 24 * time.Hour).UTC(),
		UpdatedAt: time.Now().Add(-140 * 24 * time.Hour).UTC(),
	}
}

func samplePorts() []dto.PortAllocation {
	released := time.Now().Add(-6 * time.Minute).UTC()
	return []dto.PortAllocation{
		{Port: 3000, Name: "http", Project: "shop", Service: "web", EnvVar: "PORT",
			Status: dto.PortBound, AllocatedAt: time.Now().Add(-71 * time.Minute).UTC()},
		{Port: 8012, Name: "http", Project: "shop", Service: "api", EnvVar: "PORT",
			Status: dto.PortBound, AllocatedAt: time.Now().Add(-70 * time.Minute).UTC()},
		{Port: 6379, Name: "redis", Project: "shop", Service: "redis",
			Status: dto.PortBound, AllocatedAt: time.Now().Add(-72 * time.Minute).UTC()},
		{Port: 8431, Name: "http", Project: "ingest-pipeline", Service: "gateway", EnvVar: "PORT",
			Status: dto.PortBound, AllocatedAt: time.Now().Add(-5 * time.Hour).UTC()},
		// Reserved but never observed listening: DevMan warns instead of killing
		// the process, and the interface has to explain the difference.
		{Port: 3100, Name: "http", Project: "ingest-pipeline", Service: "worker", EnvVar: "PORT",
			Status: dto.PortUnverified, AllocatedAt: time.Now().Add(-26 * time.Minute).UTC()},
		{Port: 8080, Name: "http", Project: "ingest-pipeline", Service: "db", EnvVar: "PORT",
			Status: dto.PortConflict, AllocatedAt: time.Now().Add(-26 * time.Minute).UTC()},
		{Port: 5432, Name: "postgres", Project: "shop", Service: "postgres",
			Status: dto.PortReleased, AllocatedAt: time.Now().Add(-3 * time.Hour).UTC(),
			ReleasedAt: &released},
	}
}

func sampleEvents() []dto.Event {
	base := time.Now().Add(-9 * time.Minute).UTC()
	step := func(n int) time.Time { return base.Add(time.Duration(n) * 47 * time.Second) }
	return []dto.Event{
		{Seq: 141, Type: dto.EventDaemonReady, Timestamp: step(0), Message: "daemon listening on 127.0.0.1:39190"},
		{Seq: 142, Type: dto.EventServiceStarting, Timestamp: step(1), Project: "shop", Service: "redis",
			Message: "starting redis"},
		{Seq: 143, Type: dto.EventPortReserved, Timestamp: step(2), Project: "shop", Service: "web",
			Message: "reserved port 3000 for web", Data: map[string]any{"port": 3000}},
		{Seq: 144, Type: dto.EventServiceStarted, Timestamp: step(3), Project: "shop", Service: "web",
			Message: "web is running (pid 24188)", Data: map[string]any{"pid": 24188}},
		{Seq: 145, Type: dto.EventHealthChanged, Timestamp: step(4), Project: "shop", Service: "api",
			Message: "api is UNHEALTHY: context deadline exceeded after 3s",
			Data:    map[string]any{"from": "HEALTHY", "to": "UNHEALTHY"}},
		{Seq: 146, Type: dto.EventServiceCrashed, Timestamp: step(5), Project: "ingest-pipeline", Service: "worker",
			Message: "worker exited unexpectedly with code 7", Data: map[string]any{"exit_code": 7}},
		{Seq: 147, Type: dto.EventServiceRestartScheduled, Timestamp: step(6), Project: "ingest-pipeline",
			Service: "worker", Message: "restarting worker in 4s (attempt 3)",
			Data: map[string]any{"attempt": 3, "delay_ms": 4000}},
		{Seq: 148, Type: dto.EventServiceBlocked, Timestamp: step(7), Project: "ingest-pipeline", Service: "db",
			Message: "db is blocked: the Docker engine is not reachable",
			Data:    map[string]any{"code": "DOCKER_UNAVAILABLE"}},
		{Seq: 149, Type: dto.EventPortConflict, Timestamp: step(8), Project: "ingest-pipeline", Service: "db",
			Message: "port 8080 is held by another process", Data: map[string]any{"port": 8080}},
		{Seq: 150, Type: dto.EventServiceAdopted, Timestamp: step(9), Project: "ingest-pipeline", Service: "gateway",
			Message: "adopted gateway (pid 9032); log capture is detached"},
	}
}

func sampleLogs() []dto.LogRecord {
	base := time.Now().Add(-4 * time.Minute).UTC()
	line := func(n int, service, stream, message string) dto.LogRecord {
		return dto.LogRecord{
			Seq: uint64(900 + n), Timestamp: base.Add(time.Duration(n) * 6 * time.Second),
			Project: "shop", Service: service, Stream: stream, Message: message,
		}
	}
	return []dto.LogRecord{
		line(1, "api", "stdout", "INFO  starting uvicorn on http://127.0.0.1:8012"),
		line(2, "api", "stdout", "INFO  application startup complete"),
		line(3, "api", "stderr", "WARNING  slow query: SELECT * FROM orders WHERE status = 'pending' took 2841ms"),
		// A line long enough to test wrapping and horizontal overflow, which is
		// what real stack traces and webpack output look like.
		line(4, "api", "stderr",
			"ERROR  Traceback (most recent call last):\\n  File \"/app/api/routes/checkout.py\", line 148, in submit\\n    total = cart.total(include_tax=True, currency=currency, promotions=applied_promotions)\\n  File \"/app/api/domain/cart.py\", line 92, in total\\n    raise PricingUnavailable(\"the pricing service did not answer within 3s\")"),
		line(5, "api", "stdout", "INFO  retrying pricing lookup (attempt 2 of 5)"),
		line(6, "api", "stdout", "健康检查失败：上游定价服务未在 3 秒内响应"),
		line(7, "web", "stdout", "ready in 412 ms"),
		line(8, "web", "stdout", "  ➜  Local:   http://127.0.0.1:3000/"),
	}
}

// sampleConfig is what the config editor shows. It is a working configuration
// rather than a minimal one: the editor is judged on how a real file reads in it.
const sampleConfig = `version: 1

project:
  name: shop

services:
  web:
    display_name: Web
    runtime: host
    command: pnpm dev --port ${PORT}
    ports:
      - name: http
        value: auto
        preferred: 3000
        range: frontend
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/
      interval: 2s

  api:
    display_name: API
    runtime: host
    cwd: api
    command: python
    args: [-m, uvicorn, app:app, --host, 127.0.0.1, --port, "${PORT}"]
    env:
      PYTHONUNBUFFERED: "1"
    env_files: [.env.local]
    depends_on:
      redis:
        condition: healthy
    ports:
      - name: http
        value: auto
        range: backend
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 100ms
      timeout: 3s

  redis:
    display_name: Redis
    runtime: docker-compose
    compose:
      file: docker-compose.yml
      service: redis
    ports:
      - name: redis
        value: 6379
`
