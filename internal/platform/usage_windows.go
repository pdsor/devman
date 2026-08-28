//go:build windows

package platform

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	psapi                    = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS. Only WorkingSetSize is
// read; the rest is present because the struct is passed by size.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// procParents reads the machine's parent/child table from a Toolhelp snapshot.
// This is deliberately the cheap half of sampling: it needs no process handles,
// so it works for every process on the machine including ones DevMan may not
// open.
func procParents() map[int]int {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil
	}
	parents := make(map[int]int, 256)
	for {
		parents[int(entry.ProcessID)] = int(entry.ParentProcessID)
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}
	return parents
}

// procStats measures the given pids, skipping any that cannot be opened.
//
// A process owned by another user, or a protected system process, is not an
// error here: DevMan only ever asks about its own descendants, and a pid it
// cannot read contributes nothing rather than failing the whole sample.
func procStats(pids []int) map[int]ProcStat {
	out := make(map[int]ProcStat, len(pids))
	for _, pid := range pids {
		handle, err := windows.OpenProcess(
			windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
		if err != nil {
			// GetProcessMemoryInfo needs VM_READ, which a protected process
			// will refuse. CPU time alone is still worth having.
			handle, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
			if err != nil {
				continue
			}
		}
		var stat ProcStat
		var creation, exit, kernel, user windows.Filetime
		if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err == nil {
			stat.CPUTime = filetimeDuration(kernel) + filetimeDuration(user)
		}
		counters := processMemoryCounters{cb: uint32(unsafe.Sizeof(processMemoryCounters{}))}
		if ret, _, _ := procGetProcessMemoryInfo.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&counters)),
			uintptr(counters.cb),
		); ret != 0 {
			stat.RSS = uint64(counters.workingSetSize)
		}
		windows.CloseHandle(handle)
		out[pid] = stat
	}
	return out
}

// machineStat reads whole-machine CPU and physical memory.
func machineStat() (MachineStat, error) {
	stat := MachineStat{Cores: numCPU()}

	var idle, kernel, user windows.Filetime
	if ret, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	); ret == 0 {
		return MachineStat{}, err
	}
	// GetSystemTimes reports idle time inside the kernel total, so busy time is
	// what is left after taking it out.
	total := filetimeDuration(kernel) + filetimeDuration(user)
	stat.Total = total
	stat.Busy = total - filetimeDuration(idle)

	status := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if ret, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status))); ret == 0 {
		return MachineStat{}, err
	}
	stat.MemTotal = status.totalPhys
	stat.MemUsed = status.totalPhys - status.availPhys
	return stat, nil
}

// filetimeDuration reads a FILETIME used as an interval rather than a date, so
// the 1601 epoch never enters into it: the value is a count of 100ns ticks.
func filetimeDuration(ft windows.Filetime) time.Duration {
	ticks := uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
	return time.Duration(ticks) * 100 * time.Nanosecond
}
