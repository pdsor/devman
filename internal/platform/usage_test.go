package platform

import (
	"os"
	"testing"
	"time"
)

// TestSampleSelf measures the test binary itself. The numbers cannot be
// predicted, but a sampler that returns nothing for a process that is provably
// running and has provably burned CPU is broken, and that is the failure worth
// catching on all three platforms.
func TestSampleSelf(t *testing.T) {
	pid := os.Getpid()
	// Burn a little CPU so the counter has something to report even on a fast
	// machine where the test binary has barely started.
	deadline := time.Now().Add(50 * time.Millisecond)
	sum := 0
	for time.Now().Before(deadline) {
		sum++
	}
	if sum == 0 {
		t.Fatal("busy loop did not run")
	}

	trees := SampleTrees([]int{pid})
	tree, ok := trees[pid]
	if !ok {
		t.Fatalf("no entry for the current pid %d; SampleTrees must answer for every root it was given", pid)
	}
	if tree.Procs < 1 {
		t.Fatalf("found %d processes in the current tree, want at least this one", tree.Procs)
	}
	if tree.CPUTime <= 0 {
		t.Errorf("cpu time is %v after a busy loop", tree.CPUTime)
	}
	if tree.RSS == 0 {
		t.Error("resident memory is 0 for a running process")
	}
}

func TestSampleTreesReportsMissingRoots(t *testing.T) {
	// A pid that cannot exist: the answer must be a zero entry, not a missing
	// key, so a caller can tell "measured, nothing there" from "not asked".
	const impossible = -1
	trees := SampleTrees([]int{impossible})
	tree, ok := trees[impossible]
	if !ok {
		t.Fatal("a root with no processes must still have an entry")
	}
	if tree.Procs != 0 {
		t.Errorf("Procs = %d for a nonexistent pid", tree.Procs)
	}
}

func TestSampleMachine(t *testing.T) {
	stat, err := SampleMachine()
	if err != nil {
		t.Fatalf("SampleMachine: %v", err)
	}
	if stat.Cores < 1 {
		t.Errorf("Cores = %d", stat.Cores)
	}
	if stat.MemTotal == 0 {
		t.Error("MemTotal is 0")
	}
	if stat.MemUsed == 0 || stat.MemUsed > stat.MemTotal {
		t.Errorf("MemUsed = %d with MemTotal = %d", stat.MemUsed, stat.MemTotal)
	}
	if stat.Total <= 0 {
		t.Errorf("Total cpu time = %v", stat.Total)
	}
	if stat.Busy < 0 || stat.Busy > stat.Total {
		t.Errorf("Busy = %v is not within Total = %v", stat.Busy, stat.Total)
	}
}

func TestDescendantsSurvivesACycle(t *testing.T) {
	// Two reads of a changing parent table can produce a loop. Walking it must
	// terminate rather than hang the sampler.
	children := map[int][]int{
		10: {11, 12},
		11: {13},
		13: {10},
	}
	tree := descendants(children, 10)
	if len(tree) != 4 {
		t.Fatalf("walked %v, want the four distinct pids once each", tree)
	}
	seen := map[int]int{}
	for _, pid := range tree {
		seen[pid]++
	}
	for pid, count := range seen {
		if count != 1 {
			t.Errorf("pid %d appears %d times", pid, count)
		}
	}
}
