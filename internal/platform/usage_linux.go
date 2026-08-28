//go:build linux

package platform

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicks is CLK_TCK, the unit of the CPU counters in /proc. It is 100 on
// every supported Linux target, which the existing start-time parsing already
// relies on.
const clockTicks = 100

// procParents reads pid -> ppid for every visible process from /proc.
func procParents() map[int]int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	parents := make(map[int]int, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fields, ok := statFields(pid)
		if !ok {
			continue
		}
		// Overall field 4 (ppid) is index 1 once comm and state are consumed.
		if ppid, err := strconv.Atoi(fields[1]); err == nil {
			parents[pid] = ppid
		}
	}
	return parents
}

// procStats reads CPU time and resident memory from /proc/<pid>/stat.
//
// One file per process is enough for both numbers, so a sample costs a single
// read per process of interest and nothing at all for the rest of the machine.
func procStats(pids []int) map[int]ProcStat {
	out := make(map[int]ProcStat, len(pids))
	pageSize := uint64(os.Getpagesize())
	for _, pid := range pids {
		fields, ok := statFields(pid)
		if !ok {
			continue
		}
		// Indices, counting from the field after state: utime is overall field
		// 14, stime 15, rss 24.
		const (
			utimeIndex = 11
			stimeIndex = 12
			rssIndex   = 21
		)
		if len(fields) <= rssIndex {
			continue
		}
		utime, err1 := strconv.ParseUint(fields[utimeIndex], 10, 64)
		stime, err2 := strconv.ParseUint(fields[stimeIndex], 10, 64)
		pages, err3 := strconv.ParseUint(fields[rssIndex], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		out[pid] = ProcStat{
			CPUTime: time.Duration(utime+stime) * time.Second / clockTicks,
			RSS:     pages * pageSize,
		}
	}
	return out
}

// statFields returns the fields of /proc/<pid>/stat after the comm and state
// fields. comm is a process-controlled string that may contain spaces and
// parentheses, so everything up to the last ')' is skipped rather than split.
func statFields(pid int) ([]string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil, false
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return nil, false
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 2 {
		return nil, false
	}
	return fields, true
}

// machineStat reads the aggregate CPU line from /proc/stat and physical memory
// from /proc/meminfo.
func machineStat() (MachineStat, error) {
	stat := MachineStat{Cores: numCPU()}

	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return MachineStat{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var total, idle uint64
		for i, field := range fields {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				continue
			}
			total += value
			// Fields 4 and 5 are idle and iowait: time the CPU had nothing to
			// run. Everything else counts as busy.
			if i == 3 || i == 4 {
				idle += value
			}
		}
		stat.Total = time.Duration(total) * time.Second / clockTicks
		stat.Busy = time.Duration(total-idle) * time.Second / clockTicks
		break
	}

	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return MachineStat{}, err
	}
	var totalKB, availableKB uint64
	for _, line := range strings.Split(string(meminfo), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		parsed, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemTotal":
			totalKB = parsed
		case "MemAvailable":
			// MemAvailable already excludes reclaimable cache, so it is the
			// kernel's own answer to "how much can a new process have".
			availableKB = parsed
		}
	}
	stat.MemTotal = totalKB * 1024
	if totalKB >= availableKB {
		stat.MemUsed = (totalKB - availableKB) * 1024
	}
	return stat, nil
}
