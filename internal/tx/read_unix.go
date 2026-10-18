//go:build unix

package tx

import (
	"os"
	"syscall"
)

// openRead opens path with O_NOFOLLOW so a symlink swapped in after a prior
// Lstat cannot redirect the read.
func openRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
