package platform

import (
	"runtime"
	"time"
)

// This file is the read-only half of the platform layer: what a process tree is
// costing the machine right now. It exposes raw cumulative counters, never
// percentages. A percentage is a statement about an interval, so it can only be
// computed from two samples, and the layer that owns the interval (the sampler)
// is the one that owns that arithmetic.

// ProcStat is one process's cumulative resource use.
type ProcStat struct {
	// CPUTime is user plus kernel time consumed since the process started.
	CPUTime time.Duration
	// RSS is resident memory in bytes: what the process is actually keeping in
	// RAM, which is the number a developer deciding what to shut down cares
	// about. Virtual size would be far larger and would say nothing useful.
	RSS uint64
}

// TreeStat is the resource use of a service: the process DevMan started plus
// every descendant it spawned. A dev server is almost never a single process —
// `npm run dev` is a shell, a node launcher and the bundler — so reporting only
// the direct child would understate most services by an order of magnitude.
type TreeStat struct {
	CPUTime time.Duration
	RSS     uint64
	// Procs is how many processes were found and readable. Zero means the tree
	// is gone, which the caller must distinguish from an idle tree.
	Procs int
}

// MachineStat is whole-machine load with cumulative CPU counters.
type MachineStat struct {
	// Busy and Total are cumulative CPU time across all cores. The ratio of
	// their deltas is the machine's CPU utilisation over that interval.
	Busy  time.Duration
	Total time.Duration
	Cores int
	// MemTotal and MemUsed are bytes. Used is total minus what the OS considers
	// available, so cache that would be handed back under pressure does not
	// count as used.
	MemTotal uint64
	MemUsed  uint64
}

// SampleMachine reads whole-machine CPU and memory counters.
func SampleMachine() (MachineStat, error) { return machineStat() }

// SampleTrees returns the resource use of each root's process tree.
//
// Roots are sampled together because finding descendants means reading the
// machine's parent/child table, and that table should be read once per tick no
// matter how many services are running.
//
// A root that has exited, or whose processes cannot be read, comes back with
// Procs 0 rather than being omitted: the caller asked about it and the honest
// answer is "nothing found", not silence.
func SampleTrees(roots []int) map[int]TreeStat {
	out := make(map[int]TreeStat, len(roots))
	if len(roots) == 0 {
		return out
	}
	for _, root := range roots {
		out[root] = TreeStat{}
	}

	children := make(map[int][]int)
	for pid, ppid := range procParents() {
		if pid == ppid {
			// A self-parented pid would make the walk below loop forever. Only
			// pid 1 legitimately looks like this on some systems.
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}

	// Collect every pid of interest first so all of them can be measured in one
	// pass: on Windows each measurement costs a process handle, and doing that
	// per service would open the same handle repeatedly for shared ancestors.
	members := make(map[int][]int, len(roots))
	wanted := make(map[int]struct{})
	for _, root := range roots {
		tree := descendants(children, root)
		members[root] = tree
		for _, pid := range tree {
			wanted[pid] = struct{}{}
		}
	}
	pids := make([]int, 0, len(wanted))
	for pid := range wanted {
		pids = append(pids, pid)
	}

	stats := procStats(pids)
	for root, tree := range members {
		var total TreeStat
		for _, pid := range tree {
			stat, ok := stats[pid]
			if !ok {
				continue
			}
			total.CPUTime += stat.CPUTime
			total.RSS += stat.RSS
			total.Procs++
		}
		out[root] = total
	}
	return out
}

// numCPU is how many logical cores the CPU percentage is scaled against. Go's
// value already respects container CPU limits, which is the right denominator
// for a machine reading meant to be comparable with the OS task manager.
func numCPU() int { return runtime.NumCPU() }

// descendants returns root followed by every process below it.
func descendants(children map[int][]int, root int) []int {
	seen := map[int]struct{}{root: {}}
	tree := []int{root}
	for i := 0; i < len(tree); i++ {
		for _, child := range children[tree[i]] {
			if _, dup := seen[child]; dup {
				// PID reuse can make the table look cyclic between the two
				// reads that built it. Refuse to walk in circles.
				continue
			}
			seen[child] = struct{}{}
			tree = append(tree, child)
		}
	}
	return tree
}
