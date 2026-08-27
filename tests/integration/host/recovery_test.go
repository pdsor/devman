//go:build integration

package host

import (
	"strings"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/testenv"
	"github.com/devman-project/devman/pkg/dto"
)

const recoveryYAML = `version: 1

project:
  name: recovery

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

// TestDaemonDeathLeavesAnHonestState is release gate 4.
//
// The gate is deliberately not "services always survive a killed daemon",
// because that is not true on every platform and pretending otherwise would hide
// the real requirement. On Linux and macOS a service outlives the daemon that
// started it and must be adopted on the next start. On Windows a service is in
// the daemon's job object, so it dies with the daemon by design.
//
// What must hold everywhere is that the new daemon's report matches reality:
// either RUNNING and adopted with `log_capture: detached`, or CRASHED with the
// ports released. A test that skipped the second case would leave Windows
// unverified, so both branches are asserted and neither is a skip.
func TestDaemonDeathLeavesAnHonestState(t *testing.T) {
	layout := testenv.NewLayout(t)
	first := testenv.NewStack(t, layout, window)
	root := writeProject(t, recoveryYAML)

	var project dto.Project
	first.RunJSON(&project, "register", "--trust", root)

	var started dto.OperationResult
	first.RunJSON(&started, "start", "--project", root, "--wait", "30s")
	service := singleService(t, started, "api")
	if service.Status != dto.StatusRunning {
		t.Fatalf("api is %s (%s)", service.Status, service.Message)
	}
	pid := service.PID
	port := service.Ports[0].Port

	// Close without stopping anything: this is a daemon that died, not one that
	// shut down.
	first.Close(false)

	survived := platform.Alive(pid)
	// Recorded because which branch runs is a platform fact worth having in the
	// CI log: it is the difference between adoption and honest crash reporting.
	t.Logf("after the daemon died, pid %d survived: %v", pid, survived)


	second := testenv.NewStack(t, layout, window)
	result, err := second.Supervisor().Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if survived {
		if len(result.Adopted) != 1 || result.Adopted[0].Name != "api" {
			t.Fatalf("a surviving service must be adopted, got %+v", result)
		}
		var status dto.Project
		second.RunJSON(&status, "status", "--project", root)
		adopted := testenv.ServiceByName(t, status.Services, "api")
		if adopted.Status != dto.StatusRunning {
			t.Fatalf("an adopted service is %s, expected RUNNING", adopted.Status)
		}
		if adopted.PID != pid {
			t.Fatalf("adoption changed the pid from %d to %d", pid, adopted.PID)
		}
		if !adopted.Observability.Adopted {
			t.Fatal("an adopted service must say so")
		}
		// The pipes died with the daemon that created them, and claiming
		// otherwise would make `devman logs` silently useless.
		if adopted.Observability.LogCapture != dto.LogCaptureDetached {
			t.Fatalf("log capture is %s, expected detached", adopted.Observability.LogCapture)
		}

		var restarted dto.OperationResult
		second.RunJSON(&restarted, "restart", "--project", root, "--wait", "30s")
		fresh := singleService(t, restarted, "api")
		if fresh.PID == pid {
			t.Fatalf("restart kept pid %d, so the adopted process was never replaced", pid)
		}
		if fresh.Observability.Adopted {
			t.Fatal("a restarted service is no longer adopted")
		}
		if fresh.Observability.LogCapture != dto.LogCaptureAttached {
			t.Fatalf("restarting must restore log capture, got %s", fresh.Observability.LogCapture)
		}
		waitFor(t, "the adopted process to be replaced", 20*time.Second, func() bool {
			return !platform.Alive(pid)
		})
		waitFor(t, "output to be captured again", 20*time.Second, func() bool {
			app, stdout, _ := second.App(false)
			if code := app.Run([]string{"logs", "api", "--project", root, "--tail", "50"}); code != 0 {
				return false
			}
			return strings.Contains(stdout.String(), "listening on")
		})
		return
	}

	if len(result.Vanished) != 1 || result.Vanished[0].Name != "api" {
		t.Fatalf("a service that died with its daemon must be reported vanished, got %+v", result)
	}
	var status dto.Project
	second.RunJSON(&status, "status", "--project", root)
	gone := testenv.ServiceByName(t, status.Services, "api")
	// It was supposed to be running, so this is a crash, not a stop: reporting
	// STOPPED here would tell the user they asked for this.
	if gone.Status != dto.StatusCrashed {
		t.Fatalf("a vanished service is %s, expected CRASHED", gone.Status)
	}
	if gone.PID != 0 {
		t.Fatalf("a vanished service still reports pid %d", gone.PID)
	}
	if gone.Observability.LogCapture != dto.LogCaptureNone {
		t.Fatalf("log capture is %s, expected none", gone.Observability.LogCapture)
	}
	var allocations []dto.PortAllocation
	second.RunJSON(&allocations, "ports")
	if len(allocations) != 0 {
		t.Fatalf("a vanished service must not keep its reservation: %+v", allocations)
	}
	waitFor(t, "the port to be usable again", 20*time.Second, func() bool { return free(port) })

	// Recovery has to end with a working service, not just an honest report.
	var restarted dto.OperationResult
	second.RunJSON(&restarted, "start", "--project", root, "--wait", "30s")
	fresh := singleService(t, restarted, "api")
	if fresh.Status != dto.StatusRunning {
		t.Fatalf("api is %s after recovery (%s)", fresh.Status, fresh.Message)
	}
	if fresh.Observability.LogCapture != dto.LogCaptureAttached {
		t.Fatalf("log capture is %s after recovery", fresh.Observability.LogCapture)
	}
}
