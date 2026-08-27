// Package daemon is the DevMan background service: an HTTP API over loopback
// plus the discovery, authentication and reconciliation around it.
//
// The API is the only way the CLI, the GUI and the Codex skill reach DevMan
// state. Nothing parses terminal output, and every response is a stable DTO.
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/settings"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// APIVersion is the version segment of every route.
const APIVersion = "v1"

// probeTimeout bounds the liveness check of a recorded daemon port.
const probeTimeout = 750 * time.Millisecond

// Endpoint is a reachable daemon: where it is and how to authenticate to it.
type Endpoint struct {
	Info  dto.DaemonInfo
	Token string
}

// BaseURL is the root of the API.
func (e Endpoint) BaseURL() string {
	host := e.Info.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(e.Info.Port)) + "/api/" + APIVersion
}

// Discover reads daemon.json and reports whether a live daemon is behind it.
//
// A stale file is deleted rather than reported: a daemon that was killed leaves
// its discovery record behind, and every later command would otherwise try to
// talk to a port nobody is listening on.
func Discover(layout paths.Layout) (dto.DaemonInfo, bool, error) {
	data, err := os.ReadFile(layout.Daemon)
	if err != nil {
		if os.IsNotExist(err) {
			return dto.DaemonInfo{}, false, nil
		}
		return dto.DaemonInfo{}, false, errs.Wrap(errs.CodeInternal, err, "cannot read %s", layout.Daemon)
	}

	var info dto.DaemonInfo
	if err := json.Unmarshal(data, &info); err != nil {
		// An unreadable record is as useless as a stale one.
		_ = os.Remove(layout.Daemon)
		return dto.DaemonInfo{}, false, nil
	}

	if !daemonAlive(info) {
		_ = os.Remove(layout.Daemon)
		return info, false, nil
	}
	return info, true, nil
}

// daemonAlive requires both that the recorded process exists and that something
// is listening on the recorded port. The PID alone is not enough, because PIDs
// are reused.
func daemonAlive(info dto.DaemonInfo) bool {
	if info.PID <= 0 || info.Port <= 0 {
		return false
	}
	if !platform.Alive(info.PID) {
		return false
	}
	host := info.Host
	if host == "" {
		host = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(info.Port)), probeTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Resolve returns the endpoint of a running daemon.
func Resolve(layout paths.Layout) (Endpoint, error) {
	info, running, err := Discover(layout)
	if err != nil {
		return Endpoint{}, err
	}
	if !running {
		return Endpoint{}, errs.New(errs.CodeDaemonNotRunning, "the DevMan daemon is not running")
	}
	token, err := paths.ReadSecret(layout.AuthToken)
	if err != nil {
		return Endpoint{}, errs.Wrap(errs.CodeUnauthorized, err,
			"the daemon is running but its auth token could not be read")
	}
	return Endpoint{Info: info, Token: token}, nil
}

// EnsureToken loads the local API token, creating it on first use.
//
// The token lives in its own file with owner-only access. It is deliberately not
// part of daemon.json, which any process may read to find the daemon.
func EnsureToken(layout paths.Layout) (string, error) {
	if token, err := paths.ReadSecret(layout.AuthToken); err == nil && token != "" {
		return token, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", errs.Wrap(errs.CodeInternal, err, "cannot generate an auth token")
	}
	token := hex.EncodeToString(buf)
	if err := paths.WriteSecret(layout.AuthToken, []byte(token+"\n")); err != nil {
		return "", errs.Wrap(errs.CodeInternal, err, "cannot store the auth token")
	}
	return token, nil
}

// Listener owns the daemon's TCP listener and its discovery record.
type Listener struct {
	Net    net.Listener
	Info   dto.DaemonInfo
	Token  string
	layout paths.Layout
}

// Bind claims a daemon port and publishes daemon.json.
//
// The port is found by scanning the configured window and actually binding, so
// two daemons can never agree on the same port: the loser of the race simply
// fails to bind and moves on.
func Bind(layout paths.Layout, cfg *settings.Settings, version string) (*Listener, error) {
	if err := layout.EnsureDirs(); err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot create the data directory")
	}
	if _, running, err := Discover(layout); err != nil {
		return nil, err
	} else if running {
		return nil, errs.New(errs.CodeAlreadyRunning, "a DevMan daemon is already running")
	}

	token, err := EnsureToken(layout)
	if err != nil {
		return nil, err
	}

	host := cfg.Daemon.Host
	if host == "" {
		host = "127.0.0.1"
	}
	var listener net.Listener
	var port int
	for candidate := cfg.Daemon.PortStart; candidate <= cfg.Daemon.PortEnd; candidate++ {
		bound, listenErr := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(candidate)))
		if listenErr != nil {
			continue
		}
		listener, port = bound, candidate
		break
	}
	if listener == nil {
		return nil, errs.New(errs.CodePortExhausted,
			"no free daemon port in %d-%d", cfg.Daemon.PortStart, cfg.Daemon.PortEnd)
	}

	info := dto.DaemonInfo{
		PID:        os.Getpid(),
		Port:       port,
		Host:       host,
		StartedAt:  time.Now().UTC(),
		APIVersion: APIVersion,
		Version:    version,
		// On Windows graceful shutdown needs a console to deliver CTRL_BREAK.
		// Reporting the truth here means a user can see why a stop was forceful.
		GracefulSignals: platform.HasConsole(),
	}
	if err := writeInfo(layout, info); err != nil {
		listener.Close()
		return nil, err
	}
	return &Listener{Net: listener, Info: info, Token: token, layout: layout}, nil
}

func writeInfo(layout paths.Layout, info dto.DaemonInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode daemon info")
	}
	if err := os.WriteFile(layout.Daemon, append(data, '\n'), 0o600); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot write %s", layout.Daemon)
	}
	return nil
}

// Release closes the listener and removes the discovery record.
func (l *Listener) Release() error {
	var firstErr error
	if l.Net != nil {
		if err := l.Net.Close(); err != nil {
			firstErr = err
		}
	}
	if err := os.Remove(l.layout.Daemon); err != nil && !os.IsNotExist(err) && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Address is the host:port the daemon listens on.
func (l *Listener) Address() string {
	return fmt.Sprintf("%s:%d", l.Info.Host, l.Info.Port)
}
