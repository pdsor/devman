package acceptance

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/testenv"
	"github.com/devman-project/devman/pkg/dto"
)

// explainUnhealthy turns "UNHEALTHY" into something a CI log can be debugged
// from. A probe message alone cannot tell "the interpreter never listened" apart
// from "something else owns that port", and on a hosted runner those have very
// different answers, so ask both questions.
func explainUnhealthy(t *testing.T, s *testenv.Stack, root string, service dto.Service) string {
	t.Helper()
	var report strings.Builder
	app, stdout, stderr := s.App(false)
	if code := app.Run([]string{"logs", service.Name, "--project", root, "--tail", "50"}); code != 0 {
		fmt.Fprintf(&report, "logs exited %d: %s%s\n", code, stdout.String(), stderr.String())
	} else {
		fmt.Fprintf(&report, "captured output:\n%s", stdout.String())
	}
	for _, allocation := range service.Ports {
		address := fmt.Sprintf("127.0.0.1:%d", allocation.Port)
		conn, err := net.DialTimeout("tcp", address, 2*time.Second)
		if err != nil {
			fmt.Fprintf(&report, "%s (%s): nothing accepted a connection: %v\n",
				address, allocation.Status, err)
			continue
		}
		conn.Close()
		fmt.Fprintf(&report, "%s (%s): a connection was accepted, so something is listening there\n",
			address, allocation.Status)
	}
	return report.String()
}

// The other suites use the test binary as their service, which keeps them
// hermetic but also keeps them Go-shaped: the process reads PORT from the
// environment, writes unbuffered output and handles signals the way Go does.
// Most real DevMan users run a Python backend, and Python differs on all three
// counts. These fixtures cover the differences that actually bite:
//
//   - Uvicorn and Django do not read PORT. The port has to arrive as a command
//     line argument, which means ${PORT} must expand inside args, not just env.
//   - CPython block-buffers stdout when it is a pipe, so without
//     PYTHONUNBUFFERED a service's own log lines never reach DevMan while it
//     runs — they appear only when it exits, which is precisely too late.
//   - Stopping a Python process goes through the interpreter's signal handling
//     rather than Go's runtime.
//
// pythonInterpreter picks the interpreter, honouring DEVMAN_TEST_PYTHON so CI
// (and a developer with a virtual environment) can point the suite at a
// specific one.
func pythonInterpreter(t *testing.T) string {
	t.Helper()

	candidates := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		// python3.exe on Windows is usually the Microsoft Store stub, which
		// opens the Store instead of running anything.
		candidates = []string{"python", "python3"}
	}
	if explicit := strings.TrimSpace(os.Getenv("DEVMAN_TEST_PYTHON")); explicit != "" {
		candidates = []string{explicit}
	}

	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		// Being on PATH is not the same as being usable; ask it its version.
		if err := exec.Command(path, "--version").Run(); err != nil {
			continue
		}
		return filepath.ToSlash(path)
	}
	t.Skip("no usable Python interpreter on this machine")
	return ""
}

// writePythonProject writes a devman.yaml plus the Python module it runs.
func writePythonProject(t *testing.T, configTemplate, script, scriptName, interpreter string) string {
	t.Helper()
	root := t.TempDir()

	body := strings.ReplaceAll(configTemplate, "%PYTHON%", interpreter)
	if err := os.WriteFile(filepath.Join(root, "devman.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, scriptName), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// The port is declared without `env:` on purpose. This service is the Uvicorn
// shape: it is told its port as an argument and never looks at the environment,
// so a passing test proves ${PORT} reached the command line.
const pythonYAML = `version: 1

project:
  name: pyapi

services:
  api:
    display_name: API
    runtime: host
    command: '%PYTHON%'
    args: [app.py, --port, '${PORT}']
    env:
      PYTHONUNBUFFERED: "1"
    ports:
      - name: http
        value: auto
        range: backend
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 100ms
      timeout: 1s
`

// stdlibApp deliberately does not pass flush=True to print and does not read
// PORT from the environment.
const stdlibApp = `import argparse
import http.server
import signal
import socketserver
import sys

parser = argparse.ArgumentParser()
parser.add_argument("--port", type=int, required=True)
options = parser.parse_args()


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        status = 200 if self.path == "/health" else 404
        self.send_response(status)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *_args):
        pass


class Server(http.server.ThreadingHTTPServer):
    # HTTPServer.server_bind resolves the bind address with socket.getfqdn,
    # which blocks for tens of seconds on a hosted macOS runner. The socket is
    # already bound but not yet listening while that happens, so connections are
    # dropped rather than refused and the service looks alive but deaf. The
    # resolved name is only ever used to fill in a Server header, so skip it.
    def server_bind(self):
        socketserver.TCPServer.server_bind(self)
        self.server_name = "127.0.0.1"
        self.server_port = self.server_address[1]


def shutdown(*_args):
    sys.exit(0)


for name in ("SIGINT", "SIGTERM", "SIGBREAK"):
    handled = getattr(signal, name, None)
    if handled is not None:
        signal.signal(handled, shutdown)

server = Server(("127.0.0.1", options.port), Handler)
print("python app listening on %d" % options.port)
server.serve_forever()
`

// TestPythonService is the Python fixture: a service that takes its port as an
// argument, relies on PYTHONUNBUFFERED for its output to be visible, and has to
// stop cleanly through the interpreter's signal handling.
func TestPythonService(t *testing.T) {
	interpreter := pythonInterpreter(t)
	s := newStack(t, newLayout(t))
	root := writePythonProject(t, pythonYAML, stdlibApp, "app.py", interpreter)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", root)

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", root, "--wait", "30s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}

	var status dto.Project
	s.RunJSON(&status, "status", "--project", root)
	api := serviceByName(t, status.Services, "api")
	if api.Status != dto.StatusRunning {
		t.Fatalf("api is %s (%s)", api.Status, api.Message)
	}
	if api.Health.Status != dto.HealthHealthy {
		t.Fatalf("api health is %s (%s)\n%s",
			api.Health.Status, api.Health.Message, explainUnhealthy(t, s, root, api))
	}
	if len(api.Ports) != 1 {
		t.Fatalf("api has %d ports, want 1", len(api.Ports))
	}
	// BOUND is the whole point: the interpreter is listening on the port DevMan
	// chose, so ${PORT} was expanded into the argument list.
	if api.Ports[0].Status != dto.PortBound {
		t.Fatalf("port %d is %s, expected BOUND — the argument never reached the interpreter",
			api.Ports[0].Port, api.Ports[0].Status)
	}

	// The line is printed once at startup without an explicit flush. If
	// PYTHONUNBUFFERED were not honoured it would sit in CPython's stdout buffer
	// for as long as the service runs.
	waitFor(t, "the interpreter's unbuffered output to be captured", 15*time.Second, func() bool {
		app, stdout, _ := s.App(false)
		if code := app.Run([]string{"logs", "api", "--project", root, "--tail", "50"}); code != 0 {
			return false
		}
		return strings.Contains(stdout.String(), "python app listening on")
	})

	pid := api.PID
	var stopped dto.OperationResult
	s.RunJSON(&stopped, "stop", "--project", root)
	if len(stopped.Errors) != 0 {
		t.Fatalf("stop reported errors: %+v", stopped.Errors)
	}
	if svc := serviceByName(t, stopped.Services, "api"); svc.Status != dto.StatusStopped {
		t.Fatalf("api is %s after a stop", svc.Status)
	}
	waitFor(t, "the interpreter to exit", 15*time.Second, func() bool {
		return !platform.Alive(pid)
	})

	var allocations []dto.PortAllocation
	s.RunJSON(&allocations, "ports")
	if len(allocations) != 0 {
		t.Fatalf("stopping must release every reservation, %d left: %+v",
			len(allocations), allocations)
	}
}

const fastapiYAML = `version: 1

project:
  name: fastapi-demo

services:
  api:
    display_name: API
    runtime: host
    command: '%PYTHON%'
    args: [-m, uvicorn, app:app, --host, 127.0.0.1, --port, '${PORT}']
    env:
      PYTHONUNBUFFERED: "1"
    ports:
      - name: http
        value: auto
        range: backend
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 200ms
      timeout: 2s
`

const fastapiApp = `from fastapi import FastAPI

app = FastAPI()


@app.get("/health")
def health():
    return {"status": "ok"}
`

// TestFastAPIService runs the configuration from the documentation against real
// FastAPI and Uvicorn.
//
// It is skipped when the packages are not installed rather than installing them:
// a test that reaches the network to pass is a test that fails for reasons that
// have nothing to do with DevMan. CI installs them explicitly.
func TestFastAPIService(t *testing.T) {
	interpreter := pythonInterpreter(t)
	if err := exec.Command(interpreter, "-c", "import fastapi, uvicorn").Run(); err != nil {
		t.Skip("fastapi and uvicorn are not installed for this interpreter")
	}

	s := newStack(t, newLayout(t))
	root := writePythonProject(t, fastapiYAML, fastapiApp, "app.py", interpreter)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", root)

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", root, "--wait", "60s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}

	var status dto.Project
	s.RunJSON(&status, "status", "--project", root)
	api := serviceByName(t, status.Services, "api")
	if api.Status != dto.StatusRunning {
		t.Fatalf("api is %s (%s)", api.Status, api.Message)
	}
	if api.Health.Status != dto.HealthHealthy {
		t.Fatalf("api health is %s (%s)\n%s",
			api.Health.Status, api.Health.Message, explainUnhealthy(t, s, root, api))
	}
	if len(api.Ports) != 1 || api.Ports[0].Status != dto.PortBound {
		t.Fatalf("uvicorn did not bind the port DevMan assigned: %+v", api.Ports)
	}

	// Uvicorn announces itself on stderr, so this also asserts that both streams
	// are captured rather than only stdout.
	waitFor(t, "uvicorn's startup banner to be captured", 20*time.Second, func() bool {
		app, stdout, _ := s.App(false)
		if code := app.Run([]string{"logs", "api", "--project", root, "--tail", "100"}); code != 0 {
			return false
		}
		return strings.Contains(stdout.String(), "Uvicorn running on")
	})

	pid := api.PID
	var stopped dto.OperationResult
	s.RunJSON(&stopped, "stop", "--project", root)
	if len(stopped.Errors) != 0 {
		t.Fatalf("stop reported errors: %+v", stopped.Errors)
	}
	waitFor(t, "uvicorn to exit", 20*time.Second, func() bool {
		return !platform.Alive(pid)
	})
}
