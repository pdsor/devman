//go:build darwin

package platform

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// macOS has no /proc, and the APIs that would give per-process CPU and memory
// directly (proc_pid_rusage, host_statistics) are C entry points DevMan cannot
// call: the whole binary is built without CGO so that one cross-compiled
// artifact runs on any Mac. `ps` is the supported, always-present way to ask the
// same kernel the same question, and one invocation answers it for every
// process at once.
//
// The result is cached for a fraction of the sampling interval because a single
// tick asks for the parent table and then the counters; without the cache each
// tick would fork twice for the same data.

type psSnapshot struct {
	parents map[int]int
	stats   map[int]ProcStat
	takenAt time.Time
}

var (
	psMu     sync.Mutex
	psCached psSnapshot
)

const psCacheFor = 250 * time.Millisecond

func psSample() psSnapshot {
	psMu.Lock()
	defer psMu.Unlock()
	if !psCached.takenAt.IsZero() && time.Since(psCached.takenAt) < psCacheFor {
		return psCached
	}
	snapshot := psSnapshot{
		parents: make(map[int]int, 256),
		stats:   make(map[int]ProcStat, 256),
		takenAt: time.Now(),
	}
	// Empty header suffixes keep the output values-only, so a localised or
	// reordered header cannot shift the columns.
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,time=,rss=").Output()
	if err != nil {
		// Keep the previous sample rather than reporting a machine with no
		// processes on it; a failed fork is not evidence of anything.
		return psCached
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		cpu, ok := parsePSTime(fields[2])
		rssKB, err3 := strconv.ParseUint(fields[3], 10, 64)
		if err1 != nil || err2 != nil || !ok || err3 != nil {
			continue
		}
		snapshot.parents[pid] = ppid
		snapshot.stats[pid] = ProcStat{CPUTime: cpu, RSS: rssKB * 1024}
	}
	if len(snapshot.stats) == 0 {
		return psCached
	}
	psCached = snapshot
	return snapshot
}

// parsePSTime reads the [dd-][hh:]mm:ss[.ff] CPU time column.
func parsePSTime(value string) (time.Duration, bool) {
	total := time.Duration(0)
	if days, rest, found := strings.Cut(value, "-"); found {
		count, err := strconv.Atoi(days)
		if err != nil {
			return 0, false
		}
		total += time.Duration(count) * 24 * time.Hour
		value = rest
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	units := []time.Duration{time.Hour, time.Minute, time.Second}
	units = units[len(units)-len(parts):]
	for i, part := range parts {
		count, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, false
		}
		total += time.Duration(count * float64(units[i]))
	}
	return total, true
}

func procParents() map[int]int { return psSample().parents }

func procStats(pids []int) map[int]ProcStat {
	all := psSample().stats
	out := make(map[int]ProcStat, len(pids))
	for _, pid := range pids {
		if stat, ok := all[pid]; ok {
			out[pid] = stat
		}
	}
	return out
}

// machineStat reports macOS load.
//
// Busy is the sum of every visible process's CPU time, which is the only
// machine-wide CPU counter reachable without CGO. It differs from the kernel's
// own accounting in two ways worth stating plainly: kernel time not billed to
// any process is missing, and a process that exits removes its share, so the
// counter can go backwards. The sampler treats a negative delta as zero, which
// makes a burst of exiting processes read as an idle instant rather than as a
// nonsensical number.
func machineStat() (MachineStat, error) {
	stat := MachineStat{Cores: numCPU()}

	uptime, err := bootAge()
	if err != nil {
		return MachineStat{}, err
	}
	for _, proc := range psSample().stats {
		stat.Busy += proc.CPUTime
	}
	stat.Total = time.Duration(stat.Cores) * uptime

	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return MachineStat{}, err
	}
	stat.MemTotal = total
	if available, ok := availableMemory(); ok && total >= available {
		stat.MemUsed = total - available
	}
	return stat, nil
}

// bootAge is how long the machine has been up, which is the wall clock the
// cumulative CPU capacity is measured against.
func bootAge() (time.Duration, error) {
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0, err
	}
	return time.Since(time.Unix(boot.Sec, int64(boot.Usec)*1000)), nil
}

// availableMemory sums the vm_stat page classes the kernel would hand to a new
// process without evicting anything a running one still needs.
func availableMemory() (uint64, bool) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, false
	}
	pageSize := uint64(4096)
	var pages uint64
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		if size, ok := parseVMStatPageSize(line); ok {
			pageSize = size
			continue
		}
		name, value, cut := strings.Cut(line, ":")
		if !cut {
			continue
		}
		switch strings.TrimSpace(name) {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			count, err := strconv.ParseUint(strings.Trim(strings.TrimSpace(value), "."), 10, 64)
			if err != nil {
				continue
			}
			pages += count
			found = true
		}
	}
	return pages * pageSize, found
}

// parseVMStatPageSize reads the page size out of the vm_stat banner line.
func parseVMStatPageSize(line string) (uint64, bool) {
	const marker = "page size of "
	index := strings.Index(line, marker)
	if index < 0 {
		return 0, false
	}
	fields := strings.Fields(line[index+len(marker):])
	if len(fields) == 0 {
		return 0, false
	}
	size, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return size, true
}
