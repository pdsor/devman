package portmgr

import (
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/devman-project/devman/pkg/dto"
)

// osProber answers availability by attempting a real bind.
//
// The decisive checks are IPv4 loopback and IPv4 wildcard: a service bound only
// to 0.0.0.0 would otherwise look free to a 127.0.0.1-only probe, and a service
// bound only to 127.0.0.1 would look free to a wildcard-only probe. IPv6 is an
// additional signal, used only when this machine can actually listen on IPv6,
// so dual-stack quirks cannot make every port look occupied.
type osProber struct {
	ipv6Once sync.Once
	ipv6OK   bool
}

// NewOSProber returns the real prober.
func NewOSProber() Prober { return &osProber{} }

func (p *osProber) Available(port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	address := strconv.Itoa(port)
	if !canListen("tcp4", "127.0.0.1:"+address) {
		return false
	}
	if !canListen("tcp4", "0.0.0.0:"+address) {
		return false
	}
	if p.ipv6Available() && !canListen("tcp6", "[::]:"+address) {
		return false
	}
	return true
}

func (p *osProber) ipv6Available() bool {
	p.ipv6Once.Do(func() {
		listener, err := net.Listen("tcp6", "[::]:0")
		if err == nil {
			p.ipv6OK = true
			listener.Close()
		}
	})
	return p.ipv6OK
}

func canListen(network, address string) bool {
	listener, err := net.Listen(network, address)
	if err != nil {
		return false
	}
	listener.Close()
	return true
}

// Owner delegates to the platform implementation. A nil result means "occupied
// by something, but this platform cannot say what", which is a warning and
// never a hard failure.
func (p *osProber) Owner(port int) *dto.PortOwner { return lookupPortOwner(port) }

// IsListening reports whether something accepts connections on a loopback port.
// Used by the TCP health check.
func IsListening(host string, port int, timeout time.Duration) bool {
	if host == "" {
		host = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
