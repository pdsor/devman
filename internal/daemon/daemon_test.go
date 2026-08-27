package daemon_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/client"
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
	"github.com/devman-project/devman/pkg/errs"
)

// As in the supervisor tests, the test binary doubles as the service fixture.
const helperEnv = "DEVMAN_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		_ = platform.EnsureConsole()
		os.Exit(m.Run())
	case "listen":
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
		})
		go func() { _ = http.Serve(listener, mux) }()
		waitForSignal(0)
	case "graceful":
		fmt.Fprintln(os.Stdout, "ready")
		waitForSignal(7)
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

func waitForSignal(code int) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	select {
	case <-ch:
		os.Exit(code)
	case <-time.After(90 * time.Second):
		os.Exit(99)
	}
}

const projectYAML = `version: 1

project:
  name: api-app

services:
  api:
    display_name: API
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

// harness is a fully wired daemon on a temporary data directory.
type harness struct {
	t         *testing.T
	layout    paths.Layout
	server    *daemon.Server
	listener  *daemon.Listener
	settings  *settings.Settings
	projectID string
	root      string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	layout := paths.For(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	current := settings.Default()
	current.Defaults.HealthInterval = *config.NewDuration(100 * time.Millisecond)
	current.Defaults.HealthTimeout = *config.NewDuration(time.Second)
	current.Defaults.StartTimeout = *config.NewDuration(2 * time.Second)
	current.Defaults.GracefulTimeout = *config.NewDuration(3 * time.Second)
	// A narrow window of its own keeps the test off a real daemon's ports.
	current.Daemon.PortStart = 39500
	current.Daemon.PortEnd = 39549

	db, err := storage.Open(layout.Database)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := daemon.Bind(layout, current, "test")
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
		DB:       db,
		Registry: reg,
		Ports:    ports,
		Logs:     logs,
		Events:   bus,
		Runtimes: devrun.NewSet(),
		Settings: func() *settings.Settings { return current },
	})

	server := daemon.NewServer(listener, daemon.Options{
		Layout: layout, Settings: current, DB: db, Registry: reg, Ports: ports,
		Logs: logs, Events: bus, Supervisor: sup, Version: "test",
	})
	go func() { _ = server.Serve() }()

	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(projectYAML, "%COMMAND%", filepath.ToSlash(binary))
	if err := os.WriteFile(filepath.Join(root, "devman.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	record, err := reg.Register(root, true)
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{
		t: t, layout: layout, server: server, listener: listener,
		settings: current, projectID: record.ID, root: root,
	}
	t.Cleanup(func() {
		sup.StopAll()
		sup.Close()
		_ = server.GracefulShutdown()
		logs.Close()
		bus.Close()
		_ = db.Close()
	})
	return h
}

// client connects the way the CLI does: by discovering the daemon and reading
// the token file.
func (h *harness) client() *client.Client {
	h.t.Helper()
	api, err := client.Connect(h.layout)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	return api
}

// request issues a raw HTTP call so authentication and origin handling can be
// exercised without the client's own header logic in the way.
func (h *harness) request(method, path string, headers map[string]string) *http.Response {
	h.t.Helper()
	target := fmt.Sprintf("http://%s/api/%s%s", h.listener.Address(), daemon.APIVersion, path)
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	return response
}

func (h *harness) authHeaders(extra map[string]string) map[string]string {
	headers := map[string]string{"Authorization": "Bearer " + h.listener.Token}
	for key, value := range extra {
		headers[key] = value
	}
	return headers
}

func TestRequestsWithoutTheTokenAreRejected(t *testing.T) {
	h := newHarness(t)

	response := h.request(http.MethodGet, "/daemon/status", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %s", response.Status)
	}

	wrong := h.request(http.MethodGet, "/daemon/status",
		map[string]string{"Authorization": "Bearer not-the-token"})
	defer wrong.Body.Close()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a wrong token, got %s", wrong.Status)
	}
}

func TestForeignOriginIsRejectedAndLoopbackIsAllowed(t *testing.T) {
	h := newHarness(t)

	// A page on the internet must not be able to drive a local daemon that can
	// start processes.
	foreign := h.request(http.MethodGet, "/daemon/status",
		h.authHeaders(map[string]string{"Origin": "https://evil.example"}))
	defer foreign.Body.Close()
	if foreign.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected a foreign origin to be refused, got %s", foreign.Status)
	}
	if foreign.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("a refused origin must not be echoed back")
	}

	local := h.request(http.MethodGet, "/daemon/status",
		h.authHeaders(map[string]string{"Origin": "http://localhost:5173"}))
	defer local.Body.Close()
	if local.StatusCode != http.StatusOK {
		t.Fatalf("expected a loopback origin to be allowed, got %s", local.Status)
	}
	if local.Header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatalf("expected the exact origin to be echoed, got %q",
			local.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestAuthTokenIsStoredPrivately(t *testing.T) {
	h := newHarness(t)

	if _, err := os.Stat(h.layout.AuthToken); err != nil {
		t.Fatalf("the token file was not created: %v", err)
	}
	// The token must not be in daemon.json, which any process may read to find
	// the daemon.
	discovery, err := os.ReadFile(h.layout.Daemon)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(discovery), h.listener.Token) {
		t.Fatal("the auth token must never appear in daemon.json")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(h.layout.AuthToken)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("expected 0600 on the token file, got %o", mode)
		}
	}
}

func TestDiscoveryRemovesAStaleRecord(t *testing.T) {
	layout := paths.For(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// A PID that cannot be running, plus a port nobody listens on.
	stale := []byte(`{"pid":2147483646,"port":39599,"host":"127.0.0.1",` +
		`"started_at":"2020-01-01T00:00:00Z","api_version":"v1","graceful_signals":false}`)
	if err := os.WriteFile(layout.Daemon, stale, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, running, err := daemon.Discover(layout); err != nil || running {
		t.Fatalf("a stale record must not be reported as running (running=%v, err=%v)", running, err)
	}
	if _, err := os.Stat(layout.Daemon); !os.IsNotExist(err) {
		t.Fatal("a stale daemon.json must be removed so later commands do not retry a dead port")
	}
	if _, err := daemon.Resolve(layout); !errs.Is(err, errs.CodeDaemonNotRunning) {
		t.Fatalf("expected DAEMON_NOT_RUNNING, got %v", err)
	}
}

func TestBindRefusesASecondDaemon(t *testing.T) {
	h := newHarness(t)

	_, err := daemon.Bind(h.layout, h.settings, "test")
	if !errs.Is(err, errs.CodeAlreadyRunning) {
		t.Fatalf("expected ALREADY_RUNNING for a second daemon, got %v", err)
	}
}

func TestServiceLifecycleOverTheAPI(t *testing.T) {
	h := newHarness(t)
	api := h.client()

	status, err := api.StartService(h.projectID, "api")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if status.Status != dto.StatusRunning {
		t.Fatalf("expected RUNNING, got %s (%s)", status.Status, status.Message)
	}
	if len(status.Ports) != 1 || status.URL == "" {
		t.Fatalf("expected one port and a URL, got %+v", status.Ports)
	}

	// Health, logs and port state all have to be reachable through the API,
	// because the CLI and the GUI have no other source of truth.
	waitFor(t, "the service to report healthy", 5*time.Second, func() bool {
		current, err := api.Service(h.projectID, "api")
		return err == nil && current.Health.Status == dto.HealthHealthy
	})
	waitFor(t, "captured output to be readable", 5*time.Second, func() bool {
		records, err := api.Logs(h.projectID, "api", client.LogQuery{Tail: 100})
		if err != nil {
			return false
		}
		for _, record := range records {
			if strings.Contains(record.Message, "listening on") {
				return true
			}
		}
		return false
	})

	allocations, err := api.Ports()
	if err != nil {
		t.Fatal(err)
	}
	if len(allocations) != 1 {
		t.Fatalf("expected one active allocation, got %d", len(allocations))
	}
	usage, err := api.PortUsage(allocations[0].Port)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Allocation == nil || usage.Allocation.Service != "api" {
		t.Fatalf("port usage did not name the owning service: %+v", usage)
	}

	project, err := api.Project(h.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if !project.Trusted {
		t.Fatal("the project was registered with trust and must report as trusted")
	}
	if project.Status != dto.ProjectHealthy {
		t.Fatalf("expected a healthy project, got %s", project.Status)
	}

	stopped, err := api.StopService(h.projectID, "api")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Status != dto.StatusStopped || stopped.DesiredState != dto.DesiredStopped {
		t.Fatalf("expected STOPPED/STOPPED, got %s/%s", stopped.Status, stopped.DesiredState)
	}
	remaining, err := api.Ports()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("stopping must release every reservation, %d left", len(remaining))
	}
}

func TestEventStreamDeliversServiceEvents(t *testing.T) {
	h := newHarness(t)
	api := h.client()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	received := make(chan dto.Event, 64)
	go func() {
		_ = api.StreamEvents(ctx, 0, func(event dto.Event) error {
			received <- event
			return nil
		})
	}()
	// Give the subscription time to be registered before producing events.
	time.Sleep(300 * time.Millisecond)

	if _, err := api.StartService(h.projectID, "api"); err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.After(8 * time.Second)
	for {
		select {
		case event := <-received:
			if event.Type == dto.EventServiceStarted {
				if event.Service != "api" {
					t.Fatalf("unexpected service in event: %+v", event)
				}
				return
			}
		case <-deadline:
			t.Fatal("no SERVICE_STARTED event arrived on the stream")
		}
	}
}

func TestPersistedEventsSurviveForLaterQueries(t *testing.T) {
	h := newHarness(t)
	api := h.client()

	if _, err := api.StartService(h.projectID, "api"); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "events to be persisted", 5*time.Second, func() bool {
		list, err := api.Events(50)
		if err != nil {
			return false
		}
		for _, event := range list {
			if event.Type == dto.EventServiceStarted {
				return true
			}
		}
		return false
	})

	list, err := api.Events(50)
	if err != nil {
		t.Fatal(err)
	}
	// Chronological order is part of the contract: a consumer appends.
	for i := 1; i < len(list); i++ {
		if list[i].Seq < list[i-1].Seq {
			t.Fatalf("events are not in ascending order: %d then %d", list[i-1].Seq, list[i].Seq)
		}
	}
}

func TestShutdownStopsEveryService(t *testing.T) {
	h := newHarness(t)
	api := h.client()

	status, err := api.StartService(h.projectID, "api")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := status.PID

	result, err := api.Shutdown()
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if len(result.Services) == 0 {
		t.Fatal("shutdown must report the services it stopped")
	}
	select {
	case <-h.server.ShutdownRequested():
	case <-time.After(2 * time.Second):
		t.Fatal("the shutdown request was not signalled")
	}
	waitFor(t, "the service process to be gone", 5*time.Second, func() bool {
		return !platform.Alive(pid)
	})
}

func TestUnknownEndpointReportsAnError(t *testing.T) {
	h := newHarness(t)

	response := h.request(http.MethodGet, "/nope", h.authHeaders(nil))
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown endpoint, got %s", response.Status)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), string(errs.CodeInvalidRequest)) {
		t.Fatalf("expected an INVALID_REQUEST body, got %s", body)
	}
}

func TestSettingsCanBeReadAndWritten(t *testing.T) {
	h := newHarness(t)
	api := h.client()

	flat, err := api.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if flat["daemon.host"] != "127.0.0.1" {
		t.Fatalf("unexpected daemon.host: %q", flat["daemon.host"])
	}

	if err := api.SetSetting("logs.max_backups", "7"); err != nil {
		t.Fatalf("set: %v", err)
	}
	value, err := api.Setting("logs.max_backups")
	if err != nil {
		t.Fatal(err)
	}
	if value != "7" {
		t.Fatalf("expected the new value to be readable, got %q", value)
	}

	// An invalid edit must be refused rather than persisted: the daemon has to
	// remain startable from its own settings file.
	if err := api.SetSetting("daemon.host", "0.0.0.0"); err == nil {
		t.Fatal("binding the daemon to a non-loopback address must be refused")
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
