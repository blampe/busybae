//go:build !unix

package gc

import "io/fs"

// hasExtraHardlinks is a stub for non-Unix platforms. busybae's daemon
// component uses Unix sockets and is not built on Windows, but this stub
// keeps the sweep API buildable elsewhere.
func hasExtraHardlinks(fs.FileInfo) bool { return false }
