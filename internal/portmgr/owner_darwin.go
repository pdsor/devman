//go:build darwin

package portmgr

import (
	"os/exec"
	"strconv"
	"strings"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/dto"
)

// lookupPortOwner uses lsof, which ships with macOS. There is no /proc to read
// and no public API for the TCP table, so if lsof is unavailable the owner is
// simply unknown. That is a warning in the UI, never a reason to refuse a start.
func lookupPortOwner(port int) *dto.PortOwner {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil
	}
	output, err := exec.Command(lsof,
		"-nP", "-sTCP:LISTEN", "-iTCP:"+strconv.Itoa(port), "-t").Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Fields(string(output)) {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			continue
		}
		owner := &dto.PortOwner{PID: pid}
		if info, err := platform.Inspect(pid); err == nil && info.Name != "" {
			owner.Name = info.Name
		}
		return owner
	}
	return nil
}
