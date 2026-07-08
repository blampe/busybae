package daemon

import "os"

// createStaleFile creates a regular file at path so listen() sees an
// existing entry that fails to accept connections.
func createStaleFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
}
