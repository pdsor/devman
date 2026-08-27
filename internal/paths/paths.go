// Package paths resolves the DevMan data directory and the files inside it.
//
// DevMan follows OS conventions for its own state so that it behaves like a
// normal desktop application. Project configuration (devman.yaml) is a
// completely separate, fully portable concern.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// EnvHome overrides the data directory. Used for development and tests.
const EnvHome = "DEVMAN_HOME"

// File and directory names inside the data directory.
const (
	SettingsFile  = "config.yaml"
	DatabaseFile  = "devman.db"
	DaemonFile    = "daemon.json"
	AuthTokenFile = "auth-token"
	LogsDir       = "logs"
	RunDir        = "run"
)

// Layout is the resolved set of DevMan paths.
type Layout struct {
	// Home is the root data directory.
	Home string
	// Settings is the global settings file (config.yaml).
	Settings string
	// Database is the SQLite database file.
	Database string
	// Daemon is the daemon discovery file (pid + port).
	Daemon string
	// AuthToken is the local API bearer token file.
	AuthToken string
	// Logs is the root of per-project service logs.
	Logs string
	// Run holds transient runtime files such as the daemon lock.
	Run string
}

// Resolve computes the layout. DEVMAN_HOME wins when set; otherwise the
// platform convention is used:
//
//	Windows  %LOCALAPPDATA%\DevMan
//	macOS    ~/Library/Application Support/DevMan
//	Linux    $XDG_STATE_HOME/devman, else ~/.local/state/devman
func Resolve() (Layout, error) {
	home, err := resolveHome()
	if err != nil {
		return Layout{}, err
	}
	return layoutFor(home), nil
}

// For builds a layout rooted at an explicit directory. Tests use this.
func For(home string) Layout { return layoutFor(home) }

func layoutFor(home string) Layout {
	return Layout{
		Home:      home,
		Settings:  filepath.Join(home, SettingsFile),
		Database:  filepath.Join(home, DatabaseFile),
		Daemon:    filepath.Join(home, DaemonFile),
		AuthToken: filepath.Join(home, AuthTokenFile),
		Logs:      filepath.Join(home, LogsDir),
		Run:       filepath.Join(home, RunDir),
	}
}

func resolveHome() (string, error) {
	if override := os.Getenv(EnvHome); override != "" {
		return filepath.Abs(override)
	}
	switch runtime.GOOS {
	case "windows":
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return filepath.Join(base, "DevMan"), nil
		}
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(userHome, "AppData", "Local", "DevMan"), nil
	case "darwin":
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(userHome, "Library", "Application Support", "DevMan"), nil
	default:
		if base := os.Getenv("XDG_STATE_HOME"); base != "" {
			return filepath.Join(base, "devman"), nil
		}
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(userHome, ".local", "state", "devman"), nil
	}
}

// EnsureDirs creates the data directory tree with owner-only permissions.
func (l Layout) EnsureDirs() error {
	for _, dir := range []string{l.Home, l.Logs, l.Run} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// ProjectLogDir is where a project's service logs live.
func (l Layout) ProjectLogDir(projectID string) string {
	return filepath.Join(l.Logs, projectID)
}

// ServiceLogFile is the active log file for one service.
func (l Layout) ServiceLogFile(projectID, service string) string {
	return filepath.Join(l.ProjectLogDir(projectID), service+".log")
}
