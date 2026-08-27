//go:build linux

package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func procExePath(pid int) string {
	return fmt.Sprintf("/proc/%d/exe", pid)
}

// processName falls back to /proc/<pid>/comm when the exe link is unreadable,
// which happens for processes owned by another user.
func processName(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// processStartTime reads field 22 of /proc/<pid>/stat (starttime, in clock
// ticks since boot) and converts it to wall clock time using /proc/uptime.
func processStartTime(pid int) (time.Time, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, err
	}
	// The comm field may contain spaces, so parse after the closing paren.
	close := strings.LastIndexByte(string(data), ')')
	if close < 0 {
		return time.Time{}, fmt.Errorf("malformed stat for pid %d", pid)
	}
	fields := strings.Fields(string(data[close+1:]))
	// After comm and state, starttime is field 22 overall, i.e. index 19 here.
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex {
		return time.Time{}, fmt.Errorf("malformed stat for pid %d", pid)
	}
	ticks, err := strconv.ParseFloat(fields[startTimeIndex], 64)
	if err != nil {
		return time.Time{}, err
	}
	uptime, err := readUptime()
	if err != nil {
		return time.Time{}, err
	}
	const hertz = 100 // CLK_TCK is 100 on every supported Linux target
	ageSeconds := uptime - ticks/hertz
	return time.Now().Add(-time.Duration(ageSeconds * float64(time.Second))), nil
}

func readUptime() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("malformed /proc/uptime")
	}
	return strconv.ParseFloat(fields[0], 64)
}
