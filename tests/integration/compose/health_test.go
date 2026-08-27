//go:build integration

package compose

import (
	"testing"
	"time"

	"github.com/devman-project/devman/internal/testenv"
	"github.com/devman-project/devman/pkg/dto"
)

// Fixture B is about the difference between "the container is up" and "the
// service is ready", which Compose itself is careful to distinguish and which is
// the easiest thing in the world for a supervisor to blur.
//
// `dependency` reports 503 on /health for its first five seconds and declares a
// container healthcheck, so `condition: service_healthy` has something real to
// wait for. It publishes nothing: it is only reachable inside the compose
// network, which keeps DevMan's port manager out of this particular test.
const dependencyCompose = `services:
  dependency:
    image: %IMAGE%
    command: ["-ready-after", "5s"]
    healthcheck:
      test: ["CMD", "/server", "-healthcheck"]
      interval: 1s
      timeout: 2s
      retries: 30
      start_period: 500ms
  app:
    image: %IMAGE%
    depends_on:
      dependency:
        condition: service_healthy
    ports:
      - "${PORT}:80"
`

const dependencyDevman = `version: 1

project:
  name: compose-dependency

services:
  app:
    runtime: docker-compose
    compose:
      file: compose.yaml
      project: %PROJECT%
      service: app
    ports:
      - name: http
        value: auto
        range: backend
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 500ms
      timeout: 2s
`

// TestComposeWaitsForDependencyHealth proves DevMan delegates to Compose rather
// than working around it: the app's container must not exist until the
// dependency's own healthcheck has passed, which cannot happen sooner than the
// five seconds the dependency spends unready.
func TestComposeWaitsForDependencyHealth(t *testing.T) {
	requireDocker(t)

	p := newProject(t, "dependency", map[string]string{
		"compose.yaml": dependencyCompose,
		"devman.yaml":  dependencyDevman,
	})
	s := newComposeStack(t)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", p.root)

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", p.root, "--wait", "180s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}

	p.waitForState("app", "running", 60*time.Second)
	p.waitForState("dependency", "running", 60*time.Second)

	// Compose must have waited for the healthcheck, not merely for the process.
	if health := p.containerHealth("dependency"); health != "healthy" {
		t.Fatalf("the dependency container reports health %q; Compose started the app before its dependency was ready", health)
	}

	dependencyStart := p.startedAt("dependency")
	appStart := p.startedAt("app")
	if !appStart.After(dependencyStart) {
		t.Fatalf("app started at %s, dependency at %s — the order is wrong",
			appStart, dependencyStart)
	}
	// service_healthy means the gap has to cover the unready window. A tolerance
	// of one second keeps this from being a clock-precision test.
	if gap := appStart.Sub(dependencyStart); gap < 4*time.Second {
		t.Fatalf("the app started %s after its dependency, which is sooner than the dependency could become healthy — condition: service_healthy was not honoured", gap)
	}
}

// A container that is running but not ready. -ready-after is far beyond the test
// so /health answers 503 for the whole run.
const notReadyCompose = `services:
  slow:
    image: %IMAGE%
    command: ["-ready-after", "1h"]
    ports:
      - "${PORT}:80"
`

const notReadyDevman = `version: 1

project:
  name: compose-not-ready

services:
  slow:
    runtime: docker-compose
    compose:
      file: compose.yaml
      project: %PROJECT%
      service: slow
    ports:
      - name: http
        value: auto
        range: backend
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 300ms
      timeout: 2s
      retries: 2
`

// TestComposeRunningIsNotHealthy is the assertion the task singled out: a
// running container must never be reported as a healthy service.
//
// Process status and health status are two different facts about a service, and
// collapsing them is how a dashboard ends up green while the application returns
// 503 to every request.
func TestComposeRunningIsNotHealthy(t *testing.T) {
	requireDocker(t)

	p := newProject(t, "notready", map[string]string{
		"compose.yaml": notReadyCompose,
		"devman.yaml":  notReadyDevman,
	})
	s := newComposeStack(t)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", p.root)

	// The start does not have to succeed — an unhealthy service may be reported
	// either way — but the container must come up and DevMan must be honest
	// about it afterwards.
	app, _, _ := s.App(true)
	app.Run([]string{"start", "--project", p.root, "--wait", "20s"})

	p.waitForState("slow", "running", 60*time.Second)

	var svc dto.Service
	testenv.WaitFor(t, "DevMan to settle on a health verdict", 60*time.Second, func() bool {
		var status dto.Project
		s.RunJSON(&status, "status", "--project", p.root)
		svc = testenv.ServiceByName(t, status.Services, "slow")
		return svc.Health.Status == dto.HealthUnhealthy
	})

	if svc.Health.Status == dto.HealthHealthy {
		t.Fatal("a container that answers 503 must not be reported HEALTHY")
	}
	// The process is genuinely up; DevMan should say so rather than pretend the
	// service is down because a probe failed.
	if svc.Status != dto.StatusRunning {
		t.Fatalf("the container is running, but DevMan reports process status %s (%s)",
			svc.Status, svc.Message)
	}
	if health := p.containerHealth("slow"); health != "" {
		t.Fatalf("this fixture declares no container healthcheck, got %q", health)
	}
}
