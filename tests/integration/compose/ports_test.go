//go:build integration

package compose

import (
	"strings"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/testenv"
	"github.com/devman-project/devman/pkg/dto"
)

// This fixture lets Docker choose the host port instead of taking DevMan's.
// "0:80" is the compose spelling for "publish on any free host port".
const dockerChoosesCompose = `services:
  http:
    image: %IMAGE%
    ports:
      - "0:80"
`

const dockerChoosesDevman = `version: 1

project:
  name: compose-docker-port

services:
  http:
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
`

// TestComposePortIgnoredByDockerIsUnverified pins the honest answer to the case
// where the compose file does not use the port DevMan handed out.
//
// DevMan must not report BOUND for a port nothing is listening on, and it must
// not kill a container that is otherwise fine. UNVERIFIED is the whole reason
// that status exists: the service is running, and the port DevMan reserved is
// demonstrably not the port it is reachable on.
func TestComposePortIgnoredByDockerIsUnverified(t *testing.T) {
	requireDocker(t)

	p := newProject(t, "dockerport", map[string]string{
		"compose.yaml": dockerChoosesCompose,
		"devman.yaml":  dockerChoosesDevman,
	})
	s := newComposeStack(t)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", p.root)

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", p.root, "--wait", "120s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}
	p.waitForState("http", "running", 60*time.Second)

	// Docker published something, and it is not what DevMan reserved.
	reserved := testenv.SinglePort(t, started, "http")
	published := p.publishedPort("http", 80)
	if published == reserved {
		t.Skipf("docker happened to publish the reserved port %d, so this case cannot be observed", reserved)
	}

	var svc dto.Service
	testenv.WaitFor(t, "the port verdict to settle", 60*time.Second, func() bool {
		var status dto.Project
		s.RunJSON(&status, "status", "--project", p.root)
		svc = testenv.ServiceByName(t, status.Services, "http")
		return len(svc.Ports) == 1 && svc.Ports[0].Status != dto.PortReserved
	})

	if svc.Ports[0].Status == dto.PortBound {
		t.Fatalf("DevMan claims port %d is BOUND, but the container is published on %d",
			reserved, published)
	}
	if svc.Ports[0].Status != dto.PortUnverified {
		t.Fatalf("expected UNVERIFIED for a port the container ignored, got %s",
			svc.Ports[0].Status)
	}
	if svc.Status != dto.StatusRunning {
		t.Fatalf("a container that ignored the reserved port must still run, got %s (%s)",
			svc.Status, svc.Message)
	}
}

// The stateful fixture counts its startups into a named volume, so the log line
// it prints is direct evidence of whether the volume survived.
const statefulCompose = `services:
  stateful:
    image: %IMAGE%
    command: ["-state", "/data/count"]
    volumes:
      - state:/data
    ports:
      - "${PORT}:80"

volumes:
  state:
`

const statefulDevman = `version: 1

project:
  name: compose-stateful

services:
  stateful:
    runtime: docker-compose
    compose:
      file: compose.yaml
      project: %PROJECT%
      service: stateful
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

// TestComposeStopPreservesVolume is the data-safety gate.
//
// `devman stop` must behave like `docker compose stop`. If it ever became
// `docker compose down`, this test fails by showing the counter back at 1 —
// which in a real project is somebody's database.
func TestComposeStopPreservesVolume(t *testing.T) {
	requireDocker(t)

	p := newProject(t, "stateful", map[string]string{
		"compose.yaml": statefulCompose,
		"devman.yaml":  statefulDevman,
	})
	s := newComposeStack(t)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", p.root)

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", p.root, "--wait", "120s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}
	p.waitForState("stateful", "running", 60*time.Second)

	logs := func() string {
		app, stdout, _ := s.App(false)
		if code := app.Run([]string{"logs", "stateful", "--project", p.root, "--tail", "200"}); code != 0 {
			t.Fatalf("devman logs exited %d", code)
		}
		return stdout.String()
	}

	testenv.WaitFor(t, "the first startup to be recorded", 60*time.Second, func() bool {
		return strings.Contains(logs(), "state 1")
	})

	var stopped dto.OperationResult
	s.RunJSON(&stopped, "stop", "--project", p.root)
	if len(stopped.Errors) != 0 {
		t.Fatalf("stop reported errors: %+v", stopped.Errors)
	}
	p.waitForState("stateful", "exited", 60*time.Second)

	volume := p.name + "_state"
	if !volumeExists(t, volume) {
		t.Fatalf("volume %s is gone after a stop — stop must not destroy data", volume)
	}

	var restarted dto.OperationResult
	s.RunJSON(&restarted, "start", "--project", p.root, "--wait", "120s")
	if len(restarted.Errors) != 0 {
		t.Fatalf("second start reported errors: %+v", restarted.Errors)
	}
	p.waitForState("stateful", "running", 60*time.Second)

	// The counter reaching 2 proves the same volume was reattached, not recreated.
	testenv.WaitFor(t, "the second startup to see the previous state", 60*time.Second, func() bool {
		return strings.Contains(logs(), "state 2")
	})
}
