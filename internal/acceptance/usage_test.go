package acceptance

import (
	"strings"
	"testing"
	"time"

	"github.com/devman-project/devman/pkg/dto"
)

const usageYAML = `version: 1

project:
  name: usage

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

// TestServiceUsageIsReported checks the resource sampler through the whole
// chain: a real service under a real daemon, read back over the API the desktop
// uses.
//
// The values cannot be predicted, so what is asserted is what would be a bug: a
// running process reported as having no memory, a project total that disagrees
// with its own services, or a machine reading with no memory on it. A unit test
// covers the percentage arithmetic against fixed counters; this one covers the
// wiring that unit test cannot see.
func TestServiceUsageIsReported(t *testing.T) {
	s := newStack(t, newLayout(t))
	root := writeProject(t, usageYAML)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", root)

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", root, "--wait", "20s")
	if len(started.Errors) != 0 {
		t.Fatalf("start reported errors: %+v", started.Errors)
	}
	service := singleService(t, started, "api")
	if service.PID == 0 {
		t.Fatal("a host service must report a pid; without one there is no tree to sample")
	}

	// The sampler ticks once a second and needs two readings before it can state
	// a CPU percentage, so the first status may legitimately carry no usage yet.
	var status dto.Project
	waitFor(t, "the sampler to report usage for a running service", 10*time.Second, func() bool {
		status = dto.Project{}
		s.RunJSON(&status, "status", "--project", root)
		measured := serviceByName(t, status.Services, "api")
		return measured.Usage != nil
	})

	measured := serviceByName(t, status.Services, "api")
	if measured.Usage.Procs < 1 {
		t.Errorf("usage reports %d processes for a running service", measured.Usage.Procs)
	}
	if measured.Usage.MemoryBytes == 0 {
		t.Error("usage reports no resident memory for a running process")
	}
	if measured.Usage.MemoryPercent <= 0 || measured.Usage.MemoryPercent > 100 {
		t.Errorf("memory share is %.4f%%", measured.Usage.MemoryPercent)
	}
	if measured.Usage.CPUPercent < 0 || measured.Usage.CPUPercent > 100 {
		t.Errorf("cpu share is %.4f%%", measured.Usage.CPUPercent)
	}
	if measured.Usage.SampledAt.IsZero() {
		t.Error("usage has no sample time, so a stale reading could not be spotted")
	}

	// The project total is the sum of what was measured, which is the number the
	// project card shows.
	if status.Usage == nil {
		t.Fatal("a project with a measured service must have a total")
	}
	if status.Usage.MemoryBytes < measured.Usage.MemoryBytes {
		t.Errorf("project total memory %d is below its only service's %d",
			status.Usage.MemoryBytes, measured.Usage.MemoryBytes)
	}

	machine := s.Supervisor().MachineUsage()
	if machine.Cores < 1 || machine.MemoryTotalBytes == 0 {
		t.Fatalf("machine reading is not usable: %+v", machine)
	}
	if machine.MemoryUsedBytes == 0 || machine.MemoryUsedBytes > machine.MemoryTotalBytes {
		t.Errorf("machine memory used is %d of %d", machine.MemoryUsedBytes, machine.MemoryTotalBytes)
	}
	if machine.CPUPercent < 0 || machine.CPUPercent > 100 {
		t.Errorf("machine cpu is %.2f%%", machine.CPUPercent)
	}
	// A service cannot be using more of the machine's memory than the machine
	// reports as used.
	if measured.Usage.MemoryBytes > machine.MemoryUsedBytes {
		t.Errorf("service holds %d bytes but the machine reports only %d in use",
			measured.Usage.MemoryBytes, machine.MemoryUsedBytes)
	}

	// A service that was stopped has no tree, so its reading must disappear
	// rather than freeze at the last value it had while alive.
	s.Run("stop", "--project", root, "--wait", "20s")
	waitFor(t, "usage to be dropped for a stopped service", 10*time.Second, func() bool {
		var stopped dto.Project
		s.RunJSON(&stopped, "status", "--project", root)
		return serviceByName(t, stopped.Services, "api").Usage == nil
	})
}

// TestUsageIsAbsentForAnUnmeasurableService keeps the "absent, not zero" promise
// honest for a runtime with no host process of its own.
func TestUsageIsAbsentForAnUnmeasurableService(t *testing.T) {
	s := newStack(t, newLayout(t))
	root := writeProject(t, usageYAML)

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", root)

	// Never started: there is nothing to measure, and a zeroed reading would
	// claim the service is idle rather than absent.
	var status dto.Project
	s.RunJSON(&status, "status", "--project", root)
	service := serviceByName(t, status.Services, "api")
	if service.Usage != nil {
		t.Errorf("a service that was never started reports usage %+v", *service.Usage)
	}
	if status.Usage != nil {
		t.Errorf("a project with nothing running reports a total %+v", *status.Usage)
	}
	if strings.TrimSpace(service.Message) != "" {
		t.Logf("service message: %s", service.Message)
	}
}
