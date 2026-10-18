//go:build unix

package tx

import (
	"os"
	"syscall"
)

// openRead opens path with O_NOFOLLOW so a symlink swapped in after a prior
// Lstat cannot redirect the read.
func openRead(path string) (*os.File, error) {
	// Callers pass a path already confined to the workspace root.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- confined by the caller/tx root
}

// openReadRoot opens a root-relative path with O_NOFOLLOW. os.Root already
// refuses paths that escape the root (including through a symlinked parent);
// O_NOFOLLOW additionally rejects a swapped-in final symlink.
func openReadRoot(r *os.Root, name string) (*os.File, error) {
	return r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
