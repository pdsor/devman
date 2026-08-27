//go:build integration

package compose

import (
	"testing"
	"time"

	"github.com/devman-project/devman/internal/testenv"
	"github.com/devman-project/devman/pkg/dto"
)

// startAndWaitFor drives a start that is expected to end badly and returns the
// service once it reaches a terminal verdict.
//
// A failing start may be reported through the operation result, through the
// status, or both, depending on where the failure happens. What matters to a
// user — and therefore to these tests — is what `devman status` says afterwards.
func startAndWaitFor(t *testing.T, s *testenv.Stack, root, service string, want ...dto.ProcessStatus) dto.Service {
	t.Helper()

	app, _, _ := s.App(true)
	app.Run([]string{"start", "--project", root, "--wait", "60s"})

	var found dto.Service
	testenv.WaitFor(t, "the service to reach a terminal status", 90*time.Second, func() bool {
		var status dto.Project
		s.RunJSON(&status, "status", "--project", root)
		found = testenv.ServiceByName(t, status.Services, service)
		for _, candidate := range want {
			if found.Status == candidate {
				return true
			}
		}
		return false
	})
	return found
}

const failingDevman = `version: 1

project:
  name: compose-failure

services:
  http:
    runtime: docker-compose
    compose:
      file: compose.yaml
      project: %PROJECT%
      service: http
    restart:
      policy: "no"
`

// TestMissingDockerIsBlocked keeps the distinction between "DevMan is broken" and
// "this machine has no Docker".
//
// BLOCKED with DOCKER_NOT_FOUND says nothing was executed and names the missing
// prerequisite, which is a problem the user can fix. FAILED would suggest DevMan
// tried and something went wrong inside it.
func TestMissingDockerIsBlocked(t *testing.T) {
	requireDocker(t)

	p := newProject(t, "nodocker", map[string]string{
		"compose.yaml": basicCompose,
		"devman.yaml":  failingDevman,
	})
	s := newComposeStack(t)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", p.root)

	// The daemon runs in this process, so emptying PATH is enough to make docker
	// unfindable. t.Setenv restores it before the fixture cleanup runs.
	t.Setenv("PATH", t.TempDir())

	svc := startAndWaitFor(t, s, p.root, "http", dto.StatusBlocked, dto.StatusFailed)
	if svc.Status != dto.StatusBlocked {
		t.Fatalf("expected BLOCKED without docker, got %s (%s)", svc.Status, svc.Message)
	}
	if svc.Reason == nil || svc.Reason.Code != "DOCKER_NOT_FOUND" {
		t.Fatalf("expected DOCKER_NOT_FOUND, got %+v", svc.Reason)
	}
}

const unreachableDevman = `version: 1

project:
  name: compose-unreachable

services:
  http:
    runtime: docker-compose
    compose:
      file: compose.yaml
      project: %PROJECT%
      service: http
    env:
      DOCKER_HOST: tcp://127.0.0.1:1
    restart:
      policy: "no"
`

// TestDockerDaemonUnavailableIsBlocked covers the case the task singled out: the
// Docker CLI exists, the engine does not answer.
//
// Pointing DOCKER_HOST at a port nothing listens on reproduces "Docker Desktop is
// not running" deterministically, without stopping the engine this suite needs.
// The answer must be DOCKER_UNAVAILABLE — not COMMAND_NOT_FOUND, which would
// send someone off to reinstall Docker, and not INTERNAL, which says nothing at all.
func TestDockerDaemonUnavailableIsBlocked(t *testing.T) {
	requireDocker(t)

	p := newProject(t, "unreachable", map[string]string{
		"compose.yaml": basicCompose,
		"devman.yaml":  unreachableDevman,
	})
	s := newComposeStack(t)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", p.root)

	svc := startAndWaitFor(t, s, p.root, "http", dto.StatusBlocked, dto.StatusFailed)
	if svc.Reason == nil {
		t.Fatalf("a failed start must carry a machine readable reason, got %+v", svc)
	}
	if svc.Reason.Code != "DOCKER_UNAVAILABLE" {
		t.Fatalf("expected DOCKER_UNAVAILABLE for an unreachable engine, got %s (%s)",
			svc.Reason.Code, svc.Message)
	}
	if svc.Status != dto.StatusBlocked {
		t.Fatalf("an unreachable engine is a missing prerequisite, expected BLOCKED, got %s",
			svc.Status)
	}
}

const wrongServiceDevman = `version: 1

project:
  name: compose-wrong-service

services:
  http:
    runtime: docker-compose
    compose:
      file: compose.yaml
      project: %PROJECT%
      service: htpp
`

// TestUnknownComposeServiceIsRefusedAtRegister checks that a typo in
// compose.service is caught by validation instead of at start.
//
// The compose file is right there on disk; refusing to read it and letting
// `docker compose up` fail minutes later is not a defensible tradeoff.
func TestUnknownComposeServiceIsRefusedAtRegister(t *testing.T) {
	requireDocker(t)

	p := newProject(t, "wrongservice", map[string]string{
		"compose.yaml": basicCompose,
		"devman.yaml":  wrongServiceDevman,
	})
	s := newComposeStack(t)

	refusal := s.RunExpectingFailure("register", "--trust", p.root)
	if refusal.Code != "CONFIG_INVALID" {
		t.Fatalf("expected CONFIG_INVALID for a service the compose file does not declare, got %s: %s",
			refusal.Code, refusal.Message)
	}
}

// The container starts, is observable, and then exits non-zero on its own.
const crashingCompose = `services:
  crasher:
    image: %IMAGE%
    command: ["-exit-after", "3s", "-exit-code", "7"]
`

const crashingDevman = `version: 1

project:
  name: compose-crash

services:
  crasher:
    runtime: docker-compose
    compose:
      file: compose.yaml
      project: %PROJECT%
      service: crasher
    restart:
      policy: "no"
`

// TestComposeContainerCrashIsReported makes sure a dead container cannot be
// reported as RUNNING forever.
//
// DevMan supervises a compose service through `docker compose logs -f`, so this
// is really a test that the follower's exit is noticed and interpreted. A
// supervisor that keeps claiming RUNNING after the container is gone is worse
// than no supervisor.
func TestComposeContainerCrashIsReported(t *testing.T) {
	requireDocker(t)

	p := newProject(t, "crash", map[string]string{
		"compose.yaml": crashingCompose,
		"devman.yaml":  crashingDevman,
	})
	s := newComposeStack(t)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", p.root)

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", p.root, "--wait", "120s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}

	p.waitForState("crasher", "exited", 90*time.Second)

	var svc dto.Service
	testenv.WaitFor(t, "DevMan to notice the container is gone", 90*time.Second, func() bool {
		var status dto.Project
		s.RunJSON(&status, "status", "--project", p.root)
		svc = testenv.ServiceByName(t, status.Services, "crasher")
		return svc.Status != dto.StatusRunning && svc.Status != dto.StatusStarting
	})

	if svc.Status != dto.StatusCrashed && svc.Status != dto.StatusFailed {
		t.Fatalf("expected CRASHED or FAILED after the container exited, got %s (%s)",
			svc.Status, svc.Message)
	}

	// A crashed service must not keep its port reservation.
	var allocations []dto.PortAllocation
	s.RunJSON(&allocations, "ports")
	if len(allocations) != 0 {
		t.Fatalf("a crashed service must release its ports, %d left: %+v",
			len(allocations), allocations)
	}
}
