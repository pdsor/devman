// Package acceptance holds the end-to-end suites for DevMan V0.1.
//
// Every other test package checks one component. These three check the promises
// DevMan makes to a user: that the whole chain works, that two projects wanting
// the same port both start without editing a file, and that a daemon restart
// leaves the machine in an honest state.
//
// The suites drive the real CLI against a real daemon over the real HTTP API.
// Nothing is stubbed: the only concession to a test environment is a temporary
// data directory and a private daemon port range.
package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/cli"
	"github.com/devman-project/devman/internal/daemon"
	"github.com/devman-project/devman/internal/events"
	"github.com/devman-project/devman/internal/logstore"
	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/portmgr"
	"github.com/devman-project/devman/internal/registry"
	devrun "github.com/devman-project/devman/internal/runtime"
	"github.com/devman-project/devman/internal/settings"
	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/internal/supervisor"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
)

// The test binary doubles as the service fixture, so the suites need no external
// runtime (node, python) to exercise a real process with a real listening port.
const helperEnv = "DEVMAN_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		// Without a console DevMan cannot deliver CTRL_BREAK on Windows and every
		// stop degrades to a force kill, which would make the graceful path
		// untestable.
		_ = platform.EnsureConsole()
		os.Exit(m.Run())
	case "listen":
		serveUntilSignal()
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

// serveUntilSignal is the fixture service: it binds the port DevMan injected,
// answers /health, and exits 0 on a graceful signal.
func serveUntilSignal() {
	port := os.Getenv("PORT")
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot listen:", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go func() { _ = http.Serve(listener, mux) }()
	fmt.Fprintln(os.Stdout, "listening on "+port)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	select {
	case <-ch:
		os.Exit(0)
	case <-time.After(120 * time.Second):
		os.Exit(99)
	}
}

// --- harness ---

// stack is a complete daemon: storage, registry, ports, logs, events,
// supervisor and HTTP server, wired exactly as internal/daemon.Run wires them.
type stack struct {
	t        *testing.T
	layout   paths.Layout
	settings *settings.Settings
	db       *storage.DB
	listener *daemon.Listener
	server   *daemon.Server
	sup      *supervisor.Supervisor
	logs     *logstore.Manager
	bus      *events.Bus
	closed   bool
}

func newStack(t *testing.T, layout paths.Layout) *stack {
	t.Helper()

	current := settings.Default()
	current.Defaults.HealthInterval = *config.NewDuration(100 * time.Millisecond)
	current.Defaults.HealthTimeout = *config.NewDuration(time.Second)
	current.Defaults.StartTimeout = *config.NewDuration(5 * time.Second)
	current.Defaults.GracefulTimeout = *config.NewDuration(5 * time.Second)
	// A private window keeps the suites off a developer's real daemon.
	current.Daemon.PortStart = 39600
	current.Daemon.PortEnd = 39649

	db, err := storage.Open(layout.Database)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := daemon.Bind(layout, current, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	logs := logstore.NewManager(layout.Logs, logstore.DefaultOptions())
	reg := registry.New(db)
	ports := portmgr.New(db, current, nil)
	bus := events.New(func(event dto.Event) {
		_, _ = db.InsertEvent(storage.EventRecord{
			Type:        string(event.Type),
			ProjectID:   event.Project,
			ServiceName: event.Service,
			Message:     event.Message,
			Data:        event.Data,
			CreatedAt:   event.Timestamp,
		})
	})
	sup := supervisor.New(supervisor.Deps{
		DB: db, Registry: reg, Ports: ports, Logs: logs, Events: bus,
		Runtimes: devrun.NewSet(),
		Settings: func() *settings.Settings { return current },
	})
	server := daemon.NewServer(listener, daemon.Options{
		Layout: layout, Settings: current, DB: db, Registry: reg, Ports: ports,
		Logs: logs, Events: bus, Supervisor: sup, Version: "acceptance",
	})
	go func() { _ = server.Serve() }()

	s := &stack{
		t: t, layout: layout, settings: current, db: db,
		listener: listener, server: server, sup: sup, logs: logs, bus: bus,
	}
	t.Cleanup(func() { s.close(true) })
	return s
}

// close shuts the stack down. stopServices=false simulates a daemon that died
// without cleaning up, which is what the crash recovery suite needs.
func (s *stack) close(stopServices bool) {
	if s.closed {
		return
	}
	s.closed = true
	if stopServices {
		s.sup.StopAll()
	}
	s.sup.Close()
	_ = s.server.GracefulShutdown()
	s.logs.Close()
	s.bus.Close()
	_ = s.db.Close()
}

// app builds a CLI bound to this stack's data directory. The CLI discovers the
// daemon the same way it does for a user: through daemon.json and the token
// file in that directory.
func (s *stack) app(jsonOutput bool) (*cli.App, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return &cli.App{
		Version: "acceptance",
		Layout:  s.layout,
		Stdin:   strings.NewReader(""),
		Stdout:  &stdout,
		Stderr:  &stderr,
		JSON:    jsonOutput,
	}, &stdout, &stderr
}

// run executes one CLI command and fails the test if it exits non-zero.
func (s *stack) run(args ...string) string {
	s.t.Helper()
	app, stdout, stderr := s.app(false)
	if code := app.Run(args); code != 0 {
		s.t.Fatalf("devman %s exited %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// runJSON executes one CLI command with --json and decodes its output.
func (s *stack) runJSON(out any, args ...string) {
	s.t.Helper()
	app, stdout, stderr := s.app(true)
	if code := app.Run(args); code != 0 {
		s.t.Fatalf("devman --json %s exited %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		s.t.Fatalf("devman --json %s did not produce JSON: %v\n%s",
			strings.Join(args, " "), err, stdout.String())
	}
}

// runExpectingFailure executes a command that must fail, returning its exit code
// and the decoded error.
func (s *stack) runExpectingFailure(args ...string) dto.Error {
	s.t.Helper()
	app, stdout, _ := s.app(true)
	if code := app.Run(args); code == 0 {
		s.t.Fatalf("devman --json %s was expected to fail\n%s",
			strings.Join(args, " "), stdout.String())
	}
	// The JSON error object is the last line: a command may print a preview
	// before refusing.
	var payload struct {
		Error dto.Error `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			break
		}
		var candidate struct {
			Error *dto.Error `json:"error"`
		}
		if err := json.Unmarshal(value, &candidate); err == nil && candidate.Error != nil {
			payload.Error = *candidate.Error
		}
	}
	if payload.Error.Code == "" {
		s.t.Fatalf("no machine readable error in:\n%s", stdout.String())
	}
	return payload.Error
}

func newLayout(t *testing.T) paths.Layout {
	t.Helper()
	layout := paths.For(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return layout
}

// writeProject writes a devman.yaml whose services are this test binary in
// fixture mode.
func writeProject(t *testing.T, template string) string {
	t.Helper()
	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(template, "%COMMAND%", filepath.ToSlash(binary))
	if err := os.WriteFile(filepath.Join(root, "devman.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

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
	refusal := s.runExpectingFailure("register", root)
	if refusal.Code != "PROJECT_UNTRUSTED" {
		t.Fatalf("registering without approval must be refused, got %s: %s",
			refusal.Code, refusal.Message)
	}

	var project dto.Project
	s.runJSON(&project, "register", "--trust", root)
	if project.ID == "" || !project.Trusted {
		t.Fatalf("register did not return a trusted project: %+v", project)
	}

	var started dto.OperationResult
	s.runJSON(&started, "start", "--project", root, "--wait", "20s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}
	if len(started.Services) != 2 {
		t.Fatalf("expected both services to start, got %d", len(started.Services))
	}

	var status dto.Project
	s.runJSON(&status, "status", "--project", root)
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

	logs := s.run("logs", "backend", "--project", root, "--tail", "50")
	if !strings.Contains(logs, "listening on") {
		t.Fatalf("captured output is missing the service's own line:\n%s", logs)
	}

	var restarted dto.OperationResult
	s.runJSON(&restarted, "restart", "--project", root, "--wait", "20s")
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
	s.runJSON(&stopped, "stop", "--project", root)
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
	s.runJSON(&allocations, "ports")
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
	s.runJSON(&registered, "register", "--trust", firstRoot)
	s.runJSON(&registered, "register", "--trust", secondRoot)

	var first, second dto.OperationResult
	s.runJSON(&first, "start", "--project", firstRoot, "--wait", "20s")
	s.runJSON(&second, "start", "--project", secondRoot, "--wait", "20s")

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
	s.runJSON(&usage, "ports", fmt.Sprint(secondPort))
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
	first.runJSON(&project, "register", "--trust", root)

	var started dto.OperationResult
	first.runJSON(&started, "start", "--project", root, "--wait", "20s")
	pid := singleService(t, started, "api").PID
	if pid == 0 {
		t.Fatal("the service reported no pid")
	}

	// Tear the daemon down without stopping anything: this is the daemon dying,
	// not `devman daemon stop`.
	first.close(false)

	if !platform.Alive(pid) {
		// On Windows a service tree normally dies with its daemon, because the
		// job object is created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Inside
		// one test process the job handle outlives the stack, so the survivor
		// path is reachable here; if it is not, there is nothing to adopt and
		// the vanished path is the correct behaviour to check instead.
		t.Skip("the service did not survive its daemon on this platform")
	}

	second := newStack(t, layout)
	result, err := second.sup.Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(result.Adopted) != 1 || result.Adopted[0].Name != "api" {
		t.Fatalf("expected the surviving service to be adopted, got %+v", result)
	}

	var status dto.Project
	second.runJSON(&status, "status", "--project", root)
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
	second.runJSON(&restarted, "restart", "--project", root, "--wait", "20s")
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
		app, stdout, _ := second.app(false)
		if code := app.Run([]string{"logs", "api", "--project", root, "--tail", "50"}); code != 0 {
			return false
		}
		return strings.Contains(stdout.String(), "listening on")
	})
}

// --- helpers ---

func serviceByName(t *testing.T, services []dto.Service, name string) dto.Service {
	t.Helper()
	for _, svc := range services {
		if svc.Name == name {
			return svc
		}
	}
	t.Fatalf("no service named %q in %+v", name, services)
	return dto.Service{}
}

func singleService(t *testing.T, result dto.OperationResult, name string) dto.Service {
	t.Helper()
	if len(result.Errors) != 0 {
		t.Fatalf("operation reported errors: %+v", result.Errors)
	}
	return serviceByName(t, result.Services, name)
}

func singlePort(t *testing.T, result dto.OperationResult, name string) int {
	t.Helper()
	svc := singleService(t, result, name)
	if len(svc.Ports) != 1 {
		t.Fatalf("%s has %d ports, want 1", name, len(svc.Ports))
	}
	return svc.Ports[0].Port
}

func readConfig(t *testing.T, root string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "devman.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// free reports whether a port can be bound right now, so an assertion about a
// specific number can be skipped on a machine that is already using it.
func free(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
