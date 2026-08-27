//go:build integration

package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/dto"
)

// The fixture spawns a child which spawns a grandchild, and all three inherit
// the same output pipes. A stop that only signals the direct child leaves two
// processes holding the port and the pipes open — the exact leak that makes a
// developer reboot instead of debug.
const treeYAML = `version: 1

project:
  name: tree

services:
  worker:
    runtime: host
    command: '%COMMAND%'
    env:
      DEVMAN_TEST_HELPER: tree
      DEVMAN_TEST_TREE_DIR: '%TREEDIR%'
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

// TestStopTerminatesTheWholeProcessTree is release gate 2: after `devman stop`,
// no descendant of the service is left running and the port is usable again.
func TestStopTerminatesTheWholeProcessTree(t *testing.T) {
	s := newStack(t)

	treeDir := t.TempDir()
	root := writeProject(t, strings.ReplaceAll(treeYAML, "%TREEDIR%", filepath.ToSlash(treeDir)))

	var project dto.Project
	s.RunJSON(&project, "register", "--trust", root)

	var started dto.OperationResult
	s.RunJSON(&started, "start", "--project", root, "--wait", "30s")
	service := singleService(t, started, "worker")
	if service.Status != dto.StatusRunning {
		t.Fatalf("worker is %s (%s)", service.Status, service.Message)
	}
	port := service.Ports[0].Port

	// The whole tree has to exist before its termination means anything.
	pids := map[string]int{}
	waitFor(t, "the fixture to spawn three levels", 20*time.Second, func() bool {
		for depth := 0; depth < 3; depth++ {
			name := fmt.Sprintf("level%d.pid", depth)
			raw, err := os.ReadFile(filepath.Join(treeDir, name))
			if err != nil {
				return false
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil || pid == 0 {
				return false
			}
			pids[name] = pid
		}
		return true
	})
	for name, pid := range pids {
		if !platform.Alive(pid) {
			t.Fatalf("%s (pid %d) is not running before the stop", name, pid)
		}
	}
	// The reported pid must be the process DevMan actually started, otherwise a
	// user watching Task Manager cannot correlate what they see with what
	// DevMan says.
	if service.PID != pids["level0.pid"] {
		t.Fatalf("DevMan reports pid %d, the fixture root is pid %d", service.PID, pids["level0.pid"])
	}

	var stopped dto.OperationResult
	s.RunJSON(&stopped, "stop", "--project", root)
	if svc := singleService(t, stopped, "worker"); svc.Status != dto.StatusStopped {
		t.Fatalf("worker is %s after a stop", svc.Status)
	}

	for name, pid := range pids {
		waitFor(t, fmt.Sprintf("%s (pid %d) to be gone", name, pid), 20*time.Second, func() bool {
			return !platform.Alive(pid)
		})
	}

	// A port is only really free when the operating system agrees, which is the
	// difference between a released reservation and a released socket.
	waitFor(t, fmt.Sprintf("port %d to be usable again", port), 20*time.Second, func() bool {
		return free(port)
	})

	var allocations []dto.PortAllocation
	s.RunJSON(&allocations, "ports")
	if len(allocations) != 0 {
		t.Fatalf("stopping must release every reservation, %d left: %+v", len(allocations), allocations)
	}
}
