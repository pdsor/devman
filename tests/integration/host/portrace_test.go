//go:build integration

package host

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devman-project/devman/pkg/dto"
)

// Twenty projects that all prefer 3000. Sequential starts prove almost nothing
// here: the interesting failure is two concurrent reservations reading the same
// "free" port before either has bound it.
const raceYAML = `version: 1

project:
  name: race%N%

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
    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 100ms
      timeout: 1s
`

const racers = 20

// TestConcurrentStartsNeverShareAPort is release gate 3: N services that all ask
// for the same preferred port get N distinct ports, every one of them actually
// bound, and nothing is left reserved once they stop.
func TestConcurrentStartsNeverShareAPort(t *testing.T) {
	s := newStack(t)

	roots := make([]string, racers)
	for i := range roots {
		roots[i] = writeProject(t, strings.ReplaceAll(raceYAML, "%N%", fmt.Sprint(i)))
		var project dto.Project
		s.RunJSON(&project, "register", "--trust", roots[i])
	}

	type outcome struct {
		root   string
		port   int
		status dto.ProcessStatus
		err    error
	}

	// One CLI per goroutine: a *testing.T must not be touched off the test
	// goroutine, so failures are collected and asserted below.
	results := make([]outcome, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, root := range roots {
		wg.Add(1)
		go func(i int, root string) {
			defer wg.Done()
			app, stdout, stderr := s.App(true)
			<-start
			code := app.Run([]string{"start", "--project", root, "--wait", "60s"})
			if code != 0 {
				results[i] = outcome{root: root, err: fmt.Errorf("start exited %d\nstdout:\n%s\nstderr:\n%s",
					code, stdout.String(), stderr.String())}
				return
			}
			var result dto.OperationResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				results[i] = outcome{root: root, err: fmt.Errorf("start did not produce JSON: %v\n%s", err, stdout.String())}
				return
			}
			if len(result.Errors) != 0 {
				results[i] = outcome{root: root, err: fmt.Errorf("start reported errors: %+v", result.Errors)}
				return
			}
			if len(result.Services) != 1 || len(result.Services[0].Ports) != 1 {
				results[i] = outcome{root: root, err: fmt.Errorf("unexpected start result: %+v", result.Services)}
				return
			}
			results[i] = outcome{
				root:   root,
				port:   result.Services[0].Ports[0].Port,
				status: result.Services[0].Status,
			}
		}(i, root)
	}
	close(start)
	wg.Wait()

	owner := map[int]string{}
	for i, got := range results {
		if got.err != nil {
			t.Fatalf("racer %d: %v", i, got.err)
		}
		if got.status != dto.StatusRunning {
			t.Fatalf("racer %d is %s", i, got.status)
		}
		if got.port == 0 {
			t.Fatalf("racer %d got no port", i)
		}
		if previous, clash := owner[got.port]; clash {
			t.Fatalf("port %d was handed to both %s and %s", got.port, previous, got.root)
		}
		owner[got.port] = got.root
	}
	if len(owner) != racers {
		t.Fatalf("expected %d distinct ports, got %d", racers, len(owner))
	}

	// The allocation table must agree with what the API told each caller: one
	// live reservation per service, no duplicates, no leftovers from a retry.
	var allocations []dto.PortAllocation
	s.RunJSON(&allocations, "ports")
	if len(allocations) != racers {
		t.Fatalf("expected %d live allocations, got %d: %+v", racers, len(allocations), allocations)
	}
	seen := map[int]bool{}
	for _, allocation := range allocations {
		if seen[allocation.Port] {
			t.Fatalf("port %d is reserved twice: %+v", allocation.Port, allocations)
		}
		seen[allocation.Port] = true
		if _, ok := owner[allocation.Port]; !ok {
			t.Fatalf("port %d is reserved but was never reported to a caller", allocation.Port)
		}
		if allocation.Status != dto.PortBound {
			t.Fatalf("port %d is %s, expected BOUND (%s/%s)",
				allocation.Port, allocation.Status, allocation.Project, allocation.Service)
		}
		if allocation.ReleasedAt != nil {
			t.Fatalf("port %d is reserved and released at once: %+v", allocation.Port, allocation)
		}
	}

	for _, root := range roots {
		var stopped dto.OperationResult
		s.RunJSON(&stopped, "stop", "--project", root)
	}
	waitFor(t, "every reservation to be released", 30*time.Second, func() bool {
		var remaining []dto.PortAllocation
		s.RunJSON(&remaining, "ports")
		return len(remaining) == 0
	})
}
