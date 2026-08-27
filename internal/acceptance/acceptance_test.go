// Package acceptance holds the end-to-end suites for DevMan V0.1.
//
// Every other test package checks one component. These check the promises DevMan
// makes to a user: that the whole chain works, that two projects wanting the
// same port both start without editing a file, that a daemon restart leaves the
// machine in an honest state, and that a Python service behaves like one.
//
// The suites drive the real CLI against a real daemon over the real HTTP API.
// Nothing is stubbed: the only concessions to a test environment are a temporary
// data directory and a private daemon port window. The daemon itself is built by
// internal/testenv, which is shared with the integration suites so there is only
// one definition of "a real DevMan" to keep honest.
package acceptance

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/testenv"
	"github.com/devman-project/devman/pkg/dto"
)

// This package's private daemon port window. Every suite gets its own because
// `go test ./...` runs packages in parallel, and because a suite must never bind
// the port a developer's real daemon is on.
var window = testenv.PortWindow{Start: 39600, End: 39649}

// TestMain also serves the fixture: with DEVMAN_TEST_HELPER set, this binary is
// the service under supervision, so the suites need no external runtime.
func TestMain(m *testing.M) { testenv.RunMain(m) }

func newStack(t *testing.T, layout paths.Layout) *testenv.Stack {
	return testenv.NewStack(t, layout, window)
}

// The suites read better with unqualified names, and these keep the diff between
// what a test asserts and how it is set up small.
var (
	newLayout     = testenv.NewLayout
	writeProject  = testenv.WriteProject
	waitFor       = testenv.WaitFor
	serviceByName = testenv.ServiceByName
	singleService = testenv.SingleService
	singlePort    = testenv.SinglePort
	readConfig    = testenv.ReadConfig
	free          = testenv.Free
)

const fullChainYAML = `version: 1

project:
  name: fullchain

services:
  backend:
    display_name: Backend
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: listen
    ports:
      - name: http
        value: auto
        range: backend
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 100ms
      timeout: 1s
  frontend:
    display_name: Frontend
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: listen
    depends_on:
      backend:
        condition: healthy
    ports:
      - name: http
        value: auto
        range: frontend
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 100ms
      timeout: 1s
`

// TestFullChain is acceptance suite 1: register, start, ports, health, status,
// logs, restart, stop — and nothing left behind.
func TestFullChain(t *testing.T) {
	s := newStack(t, newLayout(t))
	root := writeProject(t, fullChainYAML)

	// Registration is the trust boundary. Without an explicit approval a
	// non-interactive caller must be refused rather than prompted.
	refusal := s.RunExpectingFailure("register", root)
	if refusal.Code != "PROJECT_UNTRUSTED" {
		t.Fatalf("registering without approval must be refused, got %s: %s",
			refusal.Code, refusal.Message)
	}

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", root)
	if project.ID == "" || !project.Trusted {
		t.Fatalf("register did not return a trusted project: %+v", project)
	}

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", root, "--wait", "20s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}
	if len(started.Services) != 2 {
		t.Fatalf("expected both services to start, got %d", len(started.Services))
	}

	var status dto.Project
	s.RunJSON(&status, "status", "--project", root)
	if status.Status != dto.ProjectHealthy {
		t.Fatalf("expected a HEALTHY project, got %s\n%+v", status.Status, status.Services)
	}
	pids := map[string]int{}
	for _, svc := range status.Services {
		if svc.Status != dto.StatusRunning {
			t.Fatalf("%s is %s (%s)", svc.Name, svc.Status, svc.Message)
		}
		if svc.Health.Status != dto.HealthHealthy {
			t.Fatalf("%s health is %s (%s)", svc.Name, svc.Health.Status, svc.Health.Message)
		}
		if len(svc.Ports) != 1 || svc.Ports[0].Port == 0 {
			t.Fatalf("%s did not get a port: %+v", svc.Name, svc.Ports)
		}
		// A port DevMan handed out must be observed in use, otherwise the
		// injection did not reach the process.
		if svc.Ports[0].Status != dto.PortBound {
			t.Fatalf("%s port %d is %s, expected BOUND",
				svc.Name, svc.Ports[0].Port, svc.Ports[0].Status)
		}
		if svc.URL == "" {
			t.Fatalf("%s has a port but no URL", svc.Name)
		}
		if svc.Observability.LogCapture != dto.LogCaptureAttached {
			t.Fatalf("%s log capture is %s", svc.Name, svc.Observability.LogCapture)
		}
		pids[svc.Name] = svc.PID
	}
	// The dependency was declared as "backend healthy", so the frontend must
	// have started second.
	backendStart := serviceByName(t, status.Services, "backend").StartedAt
	frontendStart := serviceByName(t, status.Services, "frontend").StartedAt
	if backendStart == nil || frontendStart == nil {
		t.Fatal("both services must report a start time")
	}
	if frontendStart.Before(*backendStart) {
		t.Fatal("the frontend started before the backend it depends on")
	}

	logs := s.Run("logs", "backend", "--project", root, "--tail", "50")
	if !strings.Contains(logs, "listening on") {
		t.Fatalf("captured output is missing the service's own line:\n%s", logs)
	}

	var restarted dto.OperationResult
	s.RunJSON(&restarted, "restart", "--project", root, "--wait", "20s")
	if len(restarted.Errors) != 0 {
		t.Fatalf("restart reported errors: %+v", restarted.Errors)
	}
	for _, svc := range restarted.Services {
		if svc.Status != dto.StatusRunning {
			t.Fatalf("%s is %s after a restart (%s)", svc.Name, svc.Status, svc.Message)
		}
		if svc.PID == pids[svc.Name] {
			t.Fatalf("%s kept pid %d, so it was not really restarted", svc.Name, svc.PID)
		}
		pids[svc.Name] = svc.PID
	}

	var stopped dto.OperationResult
	s.RunJSON(&stopped, "stop", "--project", root)
	if len(stopped.Errors) != 0 {
		t.Fatalf("stop reported errors: %+v", stopped.Errors)
	}
	for _, svc := range stopped.Services {
		if svc.Status != dto.StatusStopped {
			t.Fatalf("%s is %s after a stop", svc.Name, svc.Status)
		}
		if svc.DesiredState != dto.DesiredStopped {
			t.Fatalf("a manual stop must be remembered, %s desires %s", svc.Name, svc.DesiredState)
		}
	}
	// The whole tree must be gone, not just the direct children.
	for name, pid := range pids {
		waitFor(t, fmt.Sprintf("%s (pid %d) to disappear", name, pid), 10*time.Second, func() bool {
			return !platform.Alive(pid)
		})
	}

	var allocations []dto.PortAllocation
	s.RunJSON(&allocations, "ports")
	if len(allocations) != 0 {
		t.Fatalf("stopping must release every reservation, %d left: %+v",
			len(allocations), allocations)
	}
}

const preferredPortYAML = `version: 1

project:
  name: %NAME%

services:
  web:
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: listen
    ports:
      - name: http
        value: auto
        preferred: 3000
        range: frontend
        env: PORT
`

// TestTwoProjectsPreferringTheSamePort is acceptance suite 2: two projects that
// both want port 3000 must both start, on different ports, with no edit to
// either devman.yaml.
func TestTwoProjectsPreferringTheSamePort(t *testing.T) {
	s := newStack(t, newLayout(t))

	firstRoot := writeProject(t, strings.ReplaceAll(preferredPortYAML, "%NAME%", "alpha"))
	secondRoot := writeProject(t, strings.ReplaceAll(preferredPortYAML, "%NAME%", "beta"))

	firstConfig := readConfig(t, firstRoot)
	secondConfig := readConfig(t, secondRoot)

	var registered dto.Project
	s.RunJSON(&registered, "register", "--trust", firstRoot)
	s.RunJSON(&registered, "register", "--trust", secondRoot)

	var first, second dto.OperationResult
	s.RunJSON(&first, "start", "--project", firstRoot, "--wait", "20s")
	s.RunJSON(&second, "start", "--project", secondRoot, "--wait", "20s")

	firstPort := singlePort(t, first, "web")
	secondPort := singlePort(t, second, "web")
	if firstPort == secondPort {
		t.Fatalf("both projects were given port %d", firstPort)
	}
	// The whole point of the preference is that the first project gets what it
	// asked for when the port is free.
	if free(3000) && firstPort != 3000 {
		t.Fatalf("3000 was free, so the first project should have it, got %d", firstPort)
	}
	if secondPort < 3000 || secondPort > 3999 {
		t.Fatalf("the neighbour must come from the declared range, got %d", secondPort)
	}
	if secondPort != firstPort+1 && free(firstPort+1) {
		t.Fatalf("expected the next free neighbour after %d, got %d", firstPort, secondPort)
	}

	// Neither configuration may have been touched: DevMan resolves conflicts at
	// runtime, it does not rewrite a user's file.
	if readConfig(t, firstRoot) != firstConfig || readConfig(t, secondRoot) != secondConfig {
		t.Fatal("devman must never edit devman.yaml to resolve a port conflict")
	}

	// Both allocations must be visible to anyone asking who holds what.
	var usage dto.PortUsage
	s.RunJSON(&usage, "ports", fmt.Sprint(secondPort))
	if usage.Allocation == nil || usage.Allocation.Service != "web" {
		t.Fatalf("ports %d did not name the owning service: %+v", secondPort, usage)
	}
}

const survivorYAML = `version: 1

project:
  name: survivor

services:
  api:
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: listen
    ports:
      - name: http
        value: auto
        range: backend
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 100ms
      timeout: 1s
`

// TestCrashRecovery is acceptance suite 3: a service that outlives its daemon is
// adopted rather than forgotten or duplicated, is reported as running with
// detached log capture, and regains capture when restarted.
func TestCrashRecovery(t *testing.T) {
	layout := newLayout(t)
	first := newStack(t, layout)
	root := writeProject(t, survivorYAML)

	var project dto.Project
	first.RunJSON(&project, "register", "--trust", root)

	var started dto.OperationResult
	first.RunJSON(&started, "start", "--project", root, "--wait", "20s")
	pid := singleService(t, started, "api").PID
	if pid == 0 {
		t.Fatal("the service reported no pid")
	}

	// Tear the daemon down without stopping anything: this is the daemon dying,
	// not `devman daemon stop`.
	first.Close(false)

	if !platform.Alive(pid) {
		// On Windows a service tree normally dies with its daemon, because the
		// job object is created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Inside
		// one test process the job handle outlives the stack, so the survivor
		// path is reachable here; if it is not, there is nothing to adopt and
		// the vanished path is the correct behaviour to check instead.
		t.Skip("the service did not survive its daemon on this platform")
	}

	second := newStack(t, layout)
	result, err := second.Supervisor().Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(result.Adopted) != 1 || result.Adopted[0].Name != "api" {
		t.Fatalf("expected the surviving service to be adopted, got %+v", result)
	}

	var status dto.Project
	second.RunJSON(&status, "status", "--project", root)
	service := serviceByName(t, status.Services, "api")
	if service.Status != dto.StatusRunning {
		t.Fatalf("an adopted service must still be RUNNING, got %s", service.Status)
	}
	if service.PID != pid {
		t.Fatalf("adoption must reattach to the same process: pid %d, want %d", service.PID, pid)
	}
	if !service.Observability.Adopted {
		t.Fatal("an adopted service must say so")
	}
	if service.Observability.LogCapture != dto.LogCaptureDetached {
		t.Fatalf("log capture after adoption is %s, want detached",
			service.Observability.LogCapture)
	}
	// The port it is still using must remain accounted for, or a second project
	// could be handed the same one.
	if len(service.Ports) != 1 {
		t.Fatalf("an adopted service must keep its reservation: %+v", service.Ports)
	}
	if !strings.Contains(status.Path, filepath.Base(root)) && status.Path != root {
		t.Fatalf("status resolved the wrong project: %q", status.Path)
	}

	// Restarting is what restores full visibility, and the old process must be
	// gone afterwards rather than left running alongside the new one.
	var restarted dto.OperationResult
	second.RunJSON(&restarted, "restart", "--project", root, "--wait", "20s")
	fresh := singleService(t, restarted, "api")
	if fresh.PID == pid {
		t.Fatal("restart reused the adopted pid")
	}
	waitFor(t, "the adopted process to be replaced", 10*time.Second, func() bool {
		return !platform.Alive(pid)
	})
	if fresh.Observability.Adopted {
		t.Fatal("a restarted service is no longer adopted")
	}
	if fresh.Observability.LogCapture != dto.LogCaptureAttached {
		t.Fatalf("restarting must restore log capture, got %s", fresh.Observability.LogCapture)
	}
	waitFor(t, "output to be captured again", 10*time.Second, func() bool {
		app, stdout, _ := second.App(false)
		if code := app.Run([]string{"logs", "api", "--project", root, "--tail", "50"}); code != 0 {
			return false
		}
		return strings.Contains(stdout.String(), "listening on")
	})
}

