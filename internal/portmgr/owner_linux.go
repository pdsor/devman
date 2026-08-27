//go:build linux

package portmgr

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/dto"
)

// lookupPortOwner resolves a listening port to a PID by matching the socket
// inode from /proc/net/tcp against the open descriptors of each process.
//
// The scan can miss processes owned by other users, which is acceptable: owner
// information is best effort and a nil result only means "occupied by something
// DevMan cannot identify".
func lookupPortOwner(port int) *dto.PortOwner {
	inodes := listeningInodes(port)
	if len(inodes) == 0 {
		return nil
	}
	pid, ok := pidForInodes(inodes)
	if !ok {
		return nil
	}
	owner := &dto.PortOwner{PID: pid}
	if info, err := platform.Inspect(pid); err == nil && info.Name != "" {
		owner.Name = info.Name
	}
	return owner
}

// listeningInodes collects socket inodes in the LISTEN state for a port.
func listeningInodes(port int) map[string]bool {
	const stateListen = "0A"
	inodes := map[string]bool{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Scan() // header
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 || fields[3] != stateListen {
				continue
			}
			_, portHex, found := strings.Cut(fields[1], ":")
			if !found {
				continue
			}
			parsed, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil || int(parsed) != port {
				continue
			}
			inodes[fields[9]] = true
		}
		file.Close()
	}
	return inodes
}

func pidForInodes(inodes map[string]bool) (int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", entry.Name(), "fd")
		descriptors, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, descriptor := range descriptors {
			link, err := os.Readlink(filepath.Join(fdDir, descriptor.Name()))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
			if inodes[inode] {
				return pid, true
			}
		}
	}
	return 0, false
}
