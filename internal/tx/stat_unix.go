//go:build unix

package tx

import (
	"os"
	"syscall"
)

// hardlinked reports whether info has more than one hard link. Editing one name
// of a hardlinked inode also changes the other names, so aed refuses by default.
func hardlinked(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Nlink > 1
}
