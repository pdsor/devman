//go:build integration

package compose

import (
	"strings"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/testenv"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
)

// Fixture A is the smallest honest compose service: one image, one published
// port, one HTTP endpoint. The image is built by the suite, so the fixture needs
// neither a registry nor a bind mount — the latter would drag Windows path
// translation into a Compose test.
const basicCompose = `services:
  http:
    image: %IMAGE%
    ports:
      - "${PORT}:80"
`

const basicDevman = `version: 1

project:
  name: compose-basic

services:
  http:
    display_name: HTTP
    runtime: docker-compose
    compose:
      file: compose.yaml
      project: %PROJECT%
      service: http
    ports:
      - name: http
        value: auto
        range: backend
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/
      interval: 500ms
      timeout: 2s
`

// newComposeStack starts a daemon with timeouts a container can actually meet.
// The defaults are tuned for a local process, and a container that has to be
// created and networked is an order of magnitude slower.
func newComposeStack(t *testing.T) *testenv.Stack {
	t.Helper()
	stack := testenv.NewStack(t, testenv.NewLayout(t), window)
	stack.Settings().Defaults.StartTimeout = *config.NewDuration(90 * time.Second)
	stack.Settings().Defaults.GracefulTimeout = *config.NewDuration(20 * time.Second)
	stack.Settings().Defaults.HealthInterval = *config.NewDuration(500 * time.Millisecond)
	stack.Settings().Defaults.HealthTimeout = *config.NewDuration(3 * time.Second)
	return stack
}

// TestComposeLifecycle is the Compose gate: start, status, stop, start again and
// restart, each checked against what Compose itself reports rather than against
// DevMan's own database.
func TestComposeLifecycle(t *testing.T) {
	requireDocker(t)
	engine, composeVersion := dockerVersions(t)
	t.Logf("docker engine %s, compose %s", engine, composeVersion)

	p := newProject(t, "basic", map[string]string{
		"compose.yaml": basicCompose,
		"devman.yaml":  basicDevman,
	})
	s := newComposeStack(t)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", p.root)

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", p.root, "--wait", "120s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}

	// Compose's own view first: if Docker did not start a container, nothing
	// DevMan says about the service is worth reading.
	p.waitForState("http", "running", 60*time.Second)

	var status dto.Project
	s.RunJSON(&status, "status", "--project", p.root)
	svc := testenv.ServiceByName(t, status.Services, "http")
	if svc.Status != dto.StatusRunning {
		t.Fatalf("http is %s (%s)", svc.Status, svc.Message)
	}
	if len(svc.Ports) != 1 {
		t.Fatalf("http has %d ports, want 1", len(svc.Ports))
	}
	reserved := svc.Ports[0].Port

	// The port DevMan reserved must be the port Docker published. This is the
	// DevMan-managed contract: the port manager decides, compose interpolation
	// carries the decision into the container, and nobody re-rolls the dice.
	published := p.publishedPort("http", 80)
	if published != reserved {
		t.Fatalf("DevMan reserved %d but Docker published %d — the reservation did not reach compose",
			reserved, published)
	}
	if svc.Ports[0].Status != dto.PortBound {
		t.Fatalf("port %d is %s, expected BOUND", reserved, svc.Ports[0].Status)
	}
	if svc.URL == "" {
		t.Fatal("a compose service with a published port must report a URL")
	}

	testenv.WaitFor(t, "the compose service to become healthy", 60*time.Second, func() bool {
		var current dto.Project
		s.RunJSON(&current, "status", "--project", p.root)
		return testenv.ServiceByName(t, current.Services, "http").Health.Status == dto.HealthHealthy
	})

	// The container's own output has to reach DevMan's log store, otherwise a
	// compose service is a black box exactly when it misbehaves.
	testenv.WaitFor(t, "container output to be captured", 60*time.Second, func() bool {
		app, stdout, _ := s.App(false)
		if code := app.Run([]string{"logs", "http", "--project", p.root, "--tail", "100"}); code != 0 {
			return false
		}
		return strings.Contains(stdout.String(), "fixture listening on")
	})

	follower := svc.PID
	if follower == 0 {
		t.Fatal("a running compose service must report the pid it is supervising")
	}

	var stopped dto.OperationResult
	s.RunJSON(&stopped, "stop", "--project", p.root)
	if len(stopped.Errors) != 0 {
		t.Fatalf("stop reported errors: %+v", stopped.Errors)
	}
	if svc := testenv.ServiceByName(t, stopped.Services, "http"); svc.Status != dto.StatusStopped {
		t.Fatalf("http is %s after a stop", svc.Status)
	}

	// `devman stop` means `docker compose stop`, not `docker compose down`: the
	// container must still exist so it — and anything written inside it — can be
	// started again. This is the semantic difference that costs users data when
	// a tool gets it wrong, so it is asserted rather than assumed.
	p.waitForState("http", "exited", 60*time.Second)

	// No orphan log follower may survive the stop.
	testenv.WaitFor(t, "the log follower to exit", 30*time.Second, func() bool {
		return !platform.Alive(follower)
	})

	var allocations []dto.PortAllocation
	s.RunJSON(&allocations, "ports")
	if len(allocations) != 0 {
		t.Fatalf("stopping must release every reservation, %d left: %+v",
			len(allocations), allocations)
	}

	// Starting a stopped compose service again must work, and must produce a
	// running container rather than a second one.
	var restarted dto.OperationResult
	s.RunJSON(&restarted, "start", "--project", p.root, "--wait", "120s")
	if len(restarted.Errors) != 0 {
		t.Fatalf("second start reported errors: %+v", restarted.Errors)
	}
	p.waitForState("http", "running", 60*time.Second)
	if count := len(p.containers()); count != 1 {
		t.Fatalf("expected exactly one container for the service, got %d", count)
	}

	var cycled dto.OperationResult
	s.RunJSON(&cycled, "restart", "--project", p.root, "--wait", "120s")
	if len(cycled.Errors) != 0 {
		t.Fatalf("restart reported errors: %+v", cycled.Errors)
	}
	p.waitForState("http", "running", 60*time.Second)

	s.RunJSON(&stopped, "stop", "--project", p.root)
	p.waitForState("http", "exited", 60*time.Second)
}
