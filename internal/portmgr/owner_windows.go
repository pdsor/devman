//go:build windows

package portmgr

import (
	"unsafe"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/dto"
	"golang.org/x/sys/windows"
)

const (
	afInet                 = 2
	tcpTableOwnerPIDListen = 3
	tcpStateListen         = 2
)

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwningPID  uint32
}

// lookupPortOwner finds the PID listening on an IPv4 port using the Windows TCP
// table. This avoids shelling out to netstat.
func lookupPortOwner(port int) *dto.PortOwner {
	pid, ok := listenerPID(port)
	if !ok {
		return nil
	}
	owner := &dto.PortOwner{PID: pid}
	if info, err := platform.Inspect(pid); err == nil && info.Name != "" {
		owner.Name = info.Name
	}
	return owner
}

func listenerPID(port int) (int, bool) {
	var size uint32
	// First call sizes the buffer.
	procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0,
		afInet, tcpTableOwnerPIDListen, 0)
	if size == 0 {
		return 0, false
	}
	buffer := make([]byte, size)
	ret, _, _ := procGetExtendedTcpTable.Call(
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)), 0,
		afInet, tcpTableOwnerPIDListen, 0)
	if ret != 0 {
		return 0, false
	}

	count := *(*uint32)(unsafe.Pointer(&buffer[0]))
	rowSize := unsafe.Sizeof(mibTCPRowOwnerPID{})
	base := unsafe.Sizeof(uint32(0))
	for i := uint32(0); i < count; i++ {
		offset := base + uintptr(i)*rowSize
		if offset+rowSize > uintptr(len(buffer)) {
			break
		}
		row := (*mibTCPRowOwnerPID)(unsafe.Pointer(&buffer[offset]))
		if row.State != tcpStateListen {
			continue
		}
		// LocalPort is stored in network byte order in the low two bytes.
		rowPort := int(((row.LocalPort & 0xFF) << 8) | ((row.LocalPort >> 8) & 0xFF))
		if rowPort == port {
			return int(row.OwningPID), true
		}
	}
	return 0, false
}
