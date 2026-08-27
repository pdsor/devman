package paths

import (
	"os"
	"path/filepath"
)

// WriteSecret writes a file that only the current user can read.
//
// The auth token lives in its own file rather than inside daemon.json for
// exactly this reason: discovery information can be world readable, the
// credential must not be.
func WriteSecret(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// An existing file keeps its old permissions when reopened, so it is
	// replaced rather than truncated.
	_ = os.Remove(path)

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return restrictToOwner(path)
}

// ReadSecret reads a secret file, trimming the trailing newline a user may have
// added by editing it.
func ReadSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return trimSpace(string(data)), nil
}

func trimSpace(value string) string {
	start, end := 0, len(value)
	for start < end && isSpace(value[start]) {
		start++
	}
	for end > start && isSpace(value[end-1]) {
		end--
	}
	return value[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}
