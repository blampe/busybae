//go:build unix

package gc

import (
	"io/fs"
	"syscall"
)

// hasExtraHardlinks reports whether the file has more than one hardlink —
// i.e. some other path (typically a Bazel external repo tree) shares the
// same inode.
//
// When true, removing this cache entry does not free disk space; the inode
// stays live as long as the sibling link exists. The caller is expected to
// skip such files under the default sweep policy.
func hasExtraHardlinks(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Nlink > 1
}
