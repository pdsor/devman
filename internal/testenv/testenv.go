// Package testenv builds a complete DevMan daemon for tests.
//
// It exists because more than one test package needs a real daemon rather than
// a mocked one — the acceptance suites, the Docker Compose integration suite and
// the cross-platform host suites. Keeping one harness means those suites cannot
// drift into testing three slightly different DevMans.
//
// Nothing here is stubbed. NewStack wires storage, registry, ports, logs,
// events, the supervisor and the HTTP server exactly as internal/daemon.Run
// wires them; the only concessions to a test environment are a temporary data
// directory and a private daemon port window.
package testenv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
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

// HelperEnv switches the test binary from suite mode into fixture mode. A test
// binary that doubles as its own service needs no external runtime — node or
// python — to exercise a real process with a real listening port.
const HelperEnv = "DEVMAN_TEST_HELPER"

// TreeDirEnv is where a fixture in tree mode records the pids it spawned, so a
// test can assert on a process tree it did not create itself.
const TreeDirEnv = "DEVMAN_TEST_TREE_DIR"

// CrashAfterEnv makes a fixture exit non-zero after a delay.
const CrashAfterEnv = "DEVMAN_TEST_CRASH_AFTER"

// PortWindow is the daemon port range a suite may use. Every package gets its
// own window because `go test ./...` runs packages in parallel, and because a
// suite must never bind the port a developer's real daemon is on.
type PortWindow struct {
	Start int
	End   int
}

// RunMain is the body of a suite's TestMain.
//
// It dispatches fixture modes before running any test, so the same binary can be
// both the suite and the service under supervision.
func RunMain(m *testing.M) {
	switch os.Getenv(HelperEnv) {
	case "":
		// Without a console DevMan cannot deliver CTRL_BREAK on Windows and
		// every stop degrades to a force kill, which would make the graceful
		// path untestable.
		_ = platform.EnsureConsole()
		os.Exit(m.Run())
	case "listen":
		serveUntilSignal("")
	case "tree":
		serveTree()
	case "crash":
		crashAfterDelay()
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

// serveUntilSignal is the fixture service: it binds the port DevMan injected,
// answers /health, and exits 0 on a graceful signal.
func serveUntilSignal(label string) {
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
	if label != "" {
		fmt.Fprintln(os.Stdout, label+" listening on "+port)
	} else {
		fmt.Fprintln(os.Stdout, "listening on "+port)
	}

	awaitSignal()
}

// serveTree is the fixture for whole-tree termination: it spawns a child which
// spawns a grandchild, records every pid, and then behaves like the listen
// fixture. Killing only the direct child would leave two processes behind, which
// is the failure this fixture is built to catch.
func serveTree() {
	dir := os.Getenv(TreeDirEnv)
	depth := 0
	if raw := os.Getenv("DEVMAN_TEST_TREE_DEPTH"); raw != "" {
		depth, _ = strconv.Atoi(raw)
	}
	if dir != "" {
		name := fmt.Sprintf("level%d.pid", depth)
		_ = os.WriteFile(filepath.Join(dir, name), []byte(strconv.Itoa(os.Getpid())), 0o644)
	}

	// Two more levels below the process DevMan started.
	if depth < 2 {
		binary, err := os.Executable()
		if err == nil {
			child := exec.Command(binary)
			child.Env = append(os.Environ(), "DEVMAN_TEST_TREE_DEPTH="+strconv.Itoa(depth+1))
			// Inheriting the pipes is deliberate: if DevMan only closed the
			// direct child's handles, a grandchild holding them open would keep
			// the log pipeline from ever finishing.
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				fmt.Fprintln(os.Stderr, "cannot spawn child:", err)
			}
		}
	}

	if depth == 0 {
		serveUntilSignal("tree root")
		return
	}
	fmt.Fprintf(os.Stdout, "tree level %d up\n", depth)
	awaitSignal()
}

// crashAfterDelay is the fixture for the crash path: it starts, is observed
// healthy, and then exits non-zero on its own.
func crashAfterDelay() {
	delay := 500 * time.Millisecond
	if raw := os.Getenv(CrashAfterEnv); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			delay = parsed
		}
	}
	go func() {
		time.Sleep(delay)
		fmt.Fprintln(os.Stderr, "fixture is exiting non-zero on purpose")
		os.Exit(1)
	}()
	serveUntilSignal("crash fixture")
}

func awaitSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	select {
	case <-ch:
		os.Exit(0)
	case <-time.After(180 * time.Second):
		os.Exit(99)
	}
}

// --- harness ---

// Stack is a complete daemon: storage, registry, ports, logs, events,
// supervisor and HTTP server.
type Stack struct {
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

// NewStack starts a daemon on a private port window.
func NewStack(t *testing.T, layout paths.Layout, window PortWindow) *Stack {
	t.Helper()

	current := settings.Default()
	current.Defaults.HealthInterval = *config.NewDuration(100 * time.Millisecond)
	current.Defaults.HealthTimeout = *config.NewDuration(time.Second)
	current.Defaults.StartTimeout = *config.NewDuration(5 * time.Second)
	current.Defaults.GracefulTimeout = *config.NewDuration(5 * time.Second)
	current.Daemon.PortStart = window.Start
	current.Daemon.PortEnd = window.End

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
		DB: db, Registry: reg, Ports: ports, Logs: logs, Events: bus,
		Runtimes: devrun.NewSet(),
		Settings: func() *settings.Settings { return current },
	})
	server := daemon.NewServer(listener, daemon.Options{
		Layout: layout, Settings: current, DB: db, Registry: reg, Ports: ports,
		Logs: logs, Events: bus, Supervisor: sup, Version: "test",
	})
	go func() { _ = server.Serve() }()

	s := &Stack{
		t: t, layout: layout, settings: current, db: db,
		listener: listener, server: server, sup: sup, logs: logs, bus: bus,
	}
	t.Cleanup(func() { s.Close(true) })
	return s
}

// Settings is the live settings object. Mutating it before a service starts is
// how a suite gives a slower runtime — a container image, for instance — more
// time than a local process needs.
func (s *Stack) Settings() *settings.Settings { return s.settings }

// Layout is the data directory this daemon uses.
func (s *Stack) Layout() paths.Layout { return s.layout }

// Supervisor exposes the supervisor for the few assertions that have to reach
// past the API — reconciliation after a daemon restart is one, because there is
// no way to observe adoption from outside without racing it.
func (s *Stack) Supervisor() *supervisor.Supervisor { return s.sup }

// Close shuts the stack down. stopServices=false simulates a daemon that died
// without cleaning up, which is what crash recovery needs.
func (s *Stack) Close(stopServices bool) {
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

// App builds a CLI bound to this stack's data directory. The CLI discovers the
// daemon the same way it does for a user: through daemon.json and the token file.
func (s *Stack) App(jsonOutput bool) (*cli.App, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	return &cli.App{
		Version: "test",
		Layout:  s.layout,
		Stdin:   strings.NewReader(""),
		Stdout:  &stdout,
		Stderr:  &stderr,
		JSON:    jsonOutput,
	}, &stdout, &stderr
}

// Run executes one CLI command and fails the test if it exits non-zero.
func (s *Stack) Run(args ...string) string {
	s.t.Helper()
	app, stdout, stderr := s.App(false)
	if code := app.Run(args); code != 0 {
		s.t.Fatalf("devman %s exited %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// RunJSON executes one CLI command with --json and decodes its output.
func (s *Stack) RunJSON(out any, args ...string) {
	s.t.Helper()
	app, stdout, stderr := s.App(true)
	if code := app.Run(args); code != 0 {
		s.t.Fatalf("devman --json %s exited %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		s.t.Fatalf("devman --json %s did not produce JSON: %v\n%s",
			strings.Join(args, " "), err, stdout.String())
	}
}

// RunExpectingFailure executes a command that must fail and returns the decoded
// error.
func (s *Stack) RunExpectingFailure(args ...string) dto.Error {
	s.t.Helper()
	app, stdout, _ := s.App(true)
	if code := app.Run(args); code == 0 {
		s.t.Fatalf("devman --json %s was expected to fail\n%s",
			strings.Join(args, " "), stdout.String())
	}
	// The JSON error object is the last one: a command may print a preview
	// before refusing.
	var last dto.Error
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
			last = *candidate.Error
		}
	}
	if last.Code == "" {
		s.t.Fatalf("no machine readable error in:\n%s", stdout.String())
	}
	return last
}

// --- fixtures and assertions ---

// NewLayout creates a private DevMan data directory.
func NewLayout(t *testing.T) paths.Layout {
	t.Helper()
	layout := paths.For(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return layout
}

// FixtureCommand is the path to use as a service's `command`: this test binary,
// which serves as the fixture when HelperEnv is set.
func FixtureCommand(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(binary)
}

// WriteProject writes a devman.yaml in a fresh directory, replacing %COMMAND%
// with this test binary.
func WriteProject(t *testing.T, template string) string {
	t.Helper()
	return WriteProjectFiles(t, map[string]string{"devman.yaml": template})
}

// WriteProjectFiles writes several files into a fresh project directory,
// replacing %COMMAND% in each with this test binary.
func WriteProjectFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	command := FixtureCommand(t)
	for name, body := range files {
		path := filepath.Join(root, name)
		if dir := filepath.Dir(path); dir != root {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		content := strings.ReplaceAll(body, "%COMMAND%", command)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// ReadConfig returns the project's devman.yaml as written on disk, so a test can
// prove DevMan did not rewrite it.
func ReadConfig(t *testing.T, root string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "devman.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// ServiceByName fails the test when the service is absent.
func ServiceByName(t *testing.T, services []dto.Service, name string) dto.Service {
	t.Helper()
	for _, svc := range services {
		if svc.Name == name {
			return svc
		}
	}
	t.Fatalf("no service named %q in %+v", name, services)
	return dto.Service{}
}

// SingleService returns one service from an operation that must have succeeded.
func SingleService(t *testing.T, result dto.OperationResult, name string) dto.Service {
	t.Helper()
	if len(result.Errors) != 0 {
		t.Fatalf("operation reported errors: %+v", result.Errors)
	}
	return ServiceByName(t, result.Services, name)
}

// SinglePort returns the only port of a service.
func SinglePort(t *testing.T, result dto.OperationResult, name string) int {
	t.Helper()
	svc := SingleService(t, result, name)
	if len(svc.Ports) != 1 {
		t.Fatalf("%s has %d ports, want 1", name, len(svc.Ports))
	}
	return svc.Ports[0].Port
}

// Free reports whether a port can be bound right now, so an assertion about a
// specific number can be skipped on a machine already using it.
func Free(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// WaitFor polls until the condition holds or the timeout expires.
func WaitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
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
