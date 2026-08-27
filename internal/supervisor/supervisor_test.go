package supervisor

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/events"
	"github.com/devman-project/devman/internal/logstore"
	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/portmgr"
	"github.com/devman-project/devman/internal/registry"
	"github.com/devman-project/devman/internal/runtime"
	"github.com/devman-project/devman/internal/settings"
	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// The test binary is its own fixture. Re-executing it with DEVMAN_TEST_HELPER
// set produces dependency-free services that behave like real ones: binding the
// injected PORT, serving a health endpoint, exiting cleanly on a shutdown
// signal, or failing on purpose.
const (
	helperEnv    = "DEVMAN_TEST_HELPER"
	helperMarker = "DEVMAN_TEST_MARKER"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		// The daemon does this at startup: on Windows a console is required to
		// deliver CTRL_BREAK, which is how graceful shutdown works.
		_ = platform.EnsureConsole()
		os.Exit(m.Run())
	case "listen":
		runListenHelper()
	case "graceful":
		runGracefulHelper()
	case "fail":
		// Records the attempt so a test can count restarts, then fails.
		if marker := os.Getenv(helperMarker); marker != "" {
			file, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err == nil {
				fmt.Fprintln(file, "run")
				file.Close()
			}
		}
		fmt.Fprintln(os.Stderr, "failing on purpose")
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

// runListenHelper binds the port DevMan injected and serves /health there.
func runListenHelper() {
	port := os.Getenv("PORT")
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot listen:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "listening on "+port)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	go func() { _ = http.Serve(listener, mux) }()

	waitForShutdown(0)
}

// runGracefulHelper never binds anything; it only shuts down cleanly.
func runGracefulHelper() {
	fmt.Fprintln(os.Stdout, "ready")
	waitForShutdown(7)
}

func waitForShutdown(code int) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	select {
	case <-ch:
		os.Exit(code)
	case <-time.After(90 * time.Second):
		os.Exit(99)
	}
}

// harness wires a supervisor onto a temporary project, database and log
// directory.
type harness struct {
	t         *testing.T
	sup       *Supervisor
	db        *storage.DB
	registry  *registry.Registry
	ports     *portmgr.Manager
	logs      *logstore.Manager
	settings  *settings.Settings
	root      string
	projectID string
}

func newHarness(t *testing.T, yaml string) *harness {
	t.Helper()

	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(yaml, "%COMMAND%", filepath.ToSlash(binary))
	if err := os.WriteFile(filepath.Join(root, "devman.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := storage.Open(filepath.Join(t.TempDir(), "devman.db"))
	if err != nil {
		t.Fatal(err)
	}

	current := settings.Default()
	// Short timings keep the suite fast without changing any behaviour under
	// test: the state machine does not care how long a probe interval is.
	current.Defaults.HealthInterval = *config.NewDuration(100 * time.Millisecond)
	current.Defaults.HealthTimeout = *config.NewDuration(time.Second)
	current.Defaults.StartTimeout = *config.NewDuration(2 * time.Second)
	current.Defaults.GracefulTimeout = *config.NewDuration(3 * time.Second)
	current.Defaults.RestartDelay = *config.NewDuration(20 * time.Millisecond)
	current.Defaults.RestartMaxDelay = *config.NewDuration(40 * time.Millisecond)

	reg := registry.New(db)
	record, err := reg.Register(root, true)
	if err != nil {
		t.Fatal(err)
	}

	logs := logstore.NewManager(filepath.Join(t.TempDir(), "logs"), logstore.DefaultOptions())
	ports := portmgr.New(db, current, nil)

	sup := New(Deps{
		DB:       db,
		Registry: reg,
		Ports:    ports,
		Logs:     logs,
		Events:   events.New(nil),
		Runtimes: runtime.NewSet(),
		Settings: func() *settings.Settings { return current },
	})

	h := &harness{
		t: t, sup: sup, db: db, registry: reg, ports: ports, logs: logs,
		settings: current, root: root, projectID: record.ID,
	}

	t.Cleanup(func() {
		h.stopEverything()
		sup.Close()
		logs.Close()
		_ = db.Close()
	})
	return h
}

// stopEverything terminates any service still running, so a failing test cannot
// leave processes behind.
func (h *harness) stopEverything() {
	h.sup.mu.Lock()
	list := make([]*service, 0, len(h.sup.services))
	for _, sv := range h.sup.services {
		list = append(list, sv)
	}
	h.sup.mu.Unlock()
	for _, sv := range list {
		sv.opMu.Lock()
		_ = sv.stopInstance(2 * time.Second)
		sv.opMu.Unlock()
	}
}

func (h *harness) status(name string) dto.Service {
	h.t.Helper()
	status, err := h.sup.ServiceStatus(h.projectID, name)
	if err != nil {
		h.t.Fatalf("status %s: %v", name, err)
	}
	return status
}

func (h *harness) waitFor(what string, timeout time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// waitForOutput blocks until the captured log of a service contains text, which
// doubles as a check that log capture works.
func (h *harness) waitForOutput(name, text string, timeout time.Duration) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%q in the log of %s", text, name), timeout, func() bool {
		serviceLog, err := h.logs.Service(h.projectID, name)
		if err != nil {
			return false
		}
		for _, record := range serviceLog.History(logstore.Query{Tail: 50}) {
			if strings.Contains(record.Message, text) {
				return true
			}
		}
		return false
	})
}

const listenYAML = `version: 1

project:
  name: sample

services:
  web:
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: listen
    ports:
      - name: http
        value: auto
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 100ms
      timeout: 1s
`

func TestStartAllocatesPortInjectsItAndReportsHealthy(t *testing.T) {
	h := newHarness(t, listenYAML)

	status, err := h.sup.StartService(h.projectID, "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if status.Status != dto.StatusRunning {
		t.Fatalf("expected RUNNING, got %s (%s)", status.Status, status.Message)
	}
	if status.DesiredState != dto.DesiredRunning {
		t.Fatalf("desired state should be RUNNING, got %s", status.DesiredState)
	}
	if len(status.Ports) != 1 {
		t.Fatalf("expected one port allocation, got %d", len(status.Ports))
	}
	allocated := status.Ports[0].Port
	general := h.settings.Range(settings.RangeGeneral)
	if allocated < general.Start || allocated > general.End {
		t.Fatalf("port %d is outside the general range %d-%d", allocated, general.Start, general.End)
	}
	if status.Observability.LogCapture != dto.LogCaptureAttached {
		t.Fatalf("log capture should be attached, got %s", status.Observability.LogCapture)
	}

	// The helper binds exactly the port it was given, which is what proves the
	// injection reached the process.
	h.waitFor("the service to become healthy", 5*time.Second, func() bool {
		return h.status("web").Health.Status == dto.HealthHealthy
	})
	h.waitFor("the port to be observed as bound", 5*time.Second, func() bool {
		records, err := h.db.ServicePorts(h.projectID, "web")
		return err == nil && len(records) == 1 && records[0].State == storage.PortStateBound
	})

	record, err := h.db.ServiceRuntime(h.projectID, "web")
	if err != nil {
		t.Fatalf("runtime record: %v", err)
	}
	if record.DesiredState != string(dto.DesiredRunning) || record.ActualState != string(dto.StatusRunning) {
		t.Fatalf("unexpected persisted state %s/%s", record.DesiredState, record.ActualState)
	}
	if record.PID == 0 || record.SpawnedAt == nil {
		t.Fatalf("process identity was not persisted: %+v", record)
	}

	pid := status.PID
	stopped, err := h.sup.StopService(h.projectID, "web")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Status != dto.StatusStopped || stopped.DesiredState != dto.DesiredStopped {
		t.Fatalf("expected STOPPED/STOPPED, got %s/%s", stopped.Status, stopped.DesiredState)
	}
	h.waitFor("the process to disappear", 5*time.Second, func() bool {
		return !platform.Alive(pid)
	})

	active, err := h.db.ActivePorts()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("stopping must release every reservation, still holding %d", len(active))
	}
}

const gracefulYAML = `version: 1

project:
  name: sample

services:
  worker:
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: graceful
    restart:
      policy: always
      delay: 20ms
`

func TestStopIsNeverTreatedAsACrash(t *testing.T) {
	h := newHarness(t, gracefulYAML)

	if _, err := h.sup.StartService(h.projectID, "worker"); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Waiting until the service has installed its signal handler keeps the test
	// about DevMan's behaviour rather than about a race inside the fixture.
	h.waitForOutput("worker", "ready", 5*time.Second)

	if _, err := h.sup.StopService(h.projectID, "worker"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// A restart policy of `always` must not fight an explicit stop.
	time.Sleep(400 * time.Millisecond)
	status := h.status("worker")
	if status.Status != dto.StatusStopped {
		t.Fatalf("expected the service to stay STOPPED, got %s", status.Status)
	}
	if status.RestartCount != 0 {
		t.Fatalf("an explicit stop must not count as a restart, got %d", status.RestartCount)
	}
	if status.LastExitCode == nil {
		t.Fatal("the exit code of a stopped service must be recorded")
	}
	if *status.LastExitCode != 7 {
		t.Fatalf("expected the graceful exit code 7, got %d", *status.LastExitCode)
	}
}

const flakyYAML = `version: 1

project:
  name: sample

services:
  flaky:
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: fail
      DEVMAN_TEST_MARKER: '%MARKER%'
    restart:
      policy: on-failure
      max_attempts: 2
      delay: 20ms
      max_delay: 40ms
`

func TestRestartPolicyRetriesThenGivesUp(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs.log")
	h := newHarness(t, strings.ReplaceAll(flakyYAML, "%MARKER%", filepath.ToSlash(marker)))

	if _, err := h.sup.StartService(h.projectID, "flaky"); err != nil {
		t.Fatalf("start: %v", err)
	}

	// One initial run plus max_attempts restarts, and then no more.
	h.waitFor("three attempts to be recorded", 10*time.Second, func() bool {
		return countLines(marker) >= 3
	})
	h.waitFor("the service to be reported as FAILED", 5*time.Second, func() bool {
		return h.status("flaky").Status == dto.StatusFailed
	})

	time.Sleep(300 * time.Millisecond)
	if runs := countLines(marker); runs != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d", runs)
	}
	status := h.status("flaky")
	if status.RestartCount != 2 {
		t.Fatalf("expected 2 restarts, got %d", status.RestartCount)
	}
	if status.LastExitCode == nil || *status.LastExitCode != 1 {
		t.Fatalf("expected exit code 1, got %v", status.LastExitCode)
	}
	if status.DesiredState != dto.DesiredRunning {
		t.Fatalf("giving up must not rewrite the desired state, got %s", status.DesiredState)
	}
}

func countLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(data)))
}

const blockedYAML = `version: 1

project:
  name: sample

services:
  api:
    runtime: host
    command: '%COMMAND%'
    required_env: [DEVMAN_TEST_SECRET]
    env:
      DEVMAN_TEST_HELPER: graceful
`

func TestMissingRequiredEnvBlocksWithoutSpawning(t *testing.T) {
	h := newHarness(t, blockedYAML)

	_, err := h.sup.StartService(h.projectID, "api")
	if err == nil {
		t.Fatal("expected the start to be refused")
	}
	if !errs.Is(err, errs.CodeServiceBlocked) {
		t.Fatalf("expected SERVICE_BLOCKED, got %s", errs.From(err).Code)
	}

	status := h.status("api")
	if status.Status != dto.StatusBlocked {
		t.Fatalf("expected BLOCKED, got %s", status.Status)
	}
	if status.Reason == nil || status.Reason.Code != string(errs.CodeServiceBlocked) {
		t.Fatalf("the machine readable reason is missing: %+v", status.Reason)
	}
	active, err := h.db.ActivePorts()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("a blocked service must not hold ports, got %d", len(active))
	}
}

const missingCommandYAML = `version: 1

project:
  name: sample

services:
  ghost:
    runtime: host
    command: devman-not-a-real-command-xyz
    ports:
      - name: http
        value: auto
        env: PORT
`

func TestMissingExecutableIsBlockedAndReleasesPorts(t *testing.T) {
	h := newHarness(t, missingCommandYAML)

	_, err := h.sup.StartService(h.projectID, "ghost")
	if !errs.Is(err, errs.CodeCommandNotFound) {
		t.Fatalf("expected COMMAND_NOT_FOUND, got %v", err)
	}
	if status := h.status("ghost"); status.Status != dto.StatusBlocked {
		t.Fatalf("expected BLOCKED, got %s", status.Status)
	}
	active, err := h.db.ActivePorts()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("a failed preparation must not leak port reservations, got %d", len(active))
	}
}

const dependencyYAML = `version: 1

project:
  name: sample

services:
  api:
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: listen
    ports:
      - name: http
        value: auto
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 100ms
      timeout: 1s

  web:
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: graceful
    depends_on:
      api:
        condition: healthy
`

func TestProjectStartWaitsForHealthyDependency(t *testing.T) {
	h := newHarness(t, dependencyYAML)

	result, err := h.sup.StartProject(h.projectID, nil, "", true)
	if err != nil {
		t.Fatalf("start project: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", result.Errors)
	}
	if len(result.Services) != 2 {
		t.Fatalf("expected two services, got %d", len(result.Services))
	}
	// TopoOrder must put the dependency first, and the dependent may only start
	// once the dependency reported healthy.
	if result.Services[0].Name != "api" || result.Services[1].Name != "web" {
		t.Fatalf("unexpected start order: %s then %s",
			result.Services[0].Name, result.Services[1].Name)
	}
	for _, svc := range result.Services {
		if svc.Status != dto.StatusRunning {
			t.Fatalf("%s is %s: %s", svc.Name, svc.Status, svc.Message)
		}
	}
	if h.status("api").Health.Status != dto.HealthHealthy {
		t.Fatal("web started before its dependency was healthy")
	}
	if h.status("web").Health.Status != dto.HealthNotApplicable {
		t.Fatalf("a service without a probe must report N/A, got %s",
			h.status("web").Health.Status)
	}

	if _, err := h.sup.StopProject(h.projectID, nil, true); err != nil {
		t.Fatalf("stop project: %v", err)
	}
	for _, name := range []string{"api", "web"} {
		if status := h.status(name); status.Status != dto.StatusStopped {
			t.Fatalf("%s should be STOPPED, got %s", name, status.Status)
		}
	}
}

const unverifiedYAML = `version: 1

project:
  name: sample

services:
  lazy:
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: graceful
    ports:
      - name: http
        value: auto
        env: PORT
`

func TestServiceThatIgnoresItsPortStaysRunningAndUnverified(t *testing.T) {
	h := newHarness(t, unverifiedYAML)

	if _, err := h.sup.StartService(h.projectID, "lazy"); err != nil {
		t.Fatalf("start: %v", err)
	}

	h.waitFor("the allocation to be marked UNVERIFIED", 6*time.Second, func() bool {
		records, err := h.db.ServicePorts(h.projectID, "lazy")
		return err == nil && len(records) == 1 && records[0].State == storage.PortStateUnverified
	})

	// The process must be left alone: an unbound port is a warning, not a
	// reason to kill a running service.
	if status := h.status("lazy"); status.Status != dto.StatusRunning {
		t.Fatalf("expected the service to keep RUNNING, got %s", status.Status)
	}
}

func TestBackoffGrowsExponentiallyAndIsCapped(t *testing.T) {
	policy := restartPolicy{Delay: time.Second, MaxDelay: 30 * time.Second}
	expected := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

	for attempt, base := range expected {
		delay := backoff(policy, attempt)
		if delay < base {
			t.Fatalf("attempt %d: delay %s is below the base %s", attempt, delay, base)
		}
		// Jitter is bounded at 20% so the schedule stays predictable.
		if delay > base+base/5 {
			t.Fatalf("attempt %d: delay %s exceeds %s plus jitter", attempt, delay, base)
		}
	}

	capped := backoff(policy, 20)
	if capped < 30*time.Second || capped > 36*time.Second {
		t.Fatalf("the delay must be capped near max_delay, got %s", capped)
	}
}
