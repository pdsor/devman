//go:build !windows

package paths

import "os"

// restrictToOwner enforces 0600 on Unix.
func restrictToOwner(path string) error {
	return os.Chmod(path, 0o600)
}
