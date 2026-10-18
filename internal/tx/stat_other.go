//go:build !unix

package tx

import "os"

// hardlinked is a no-op on platforms without a portable link count. The
// regular-file and hash checks still apply.
func hardlinked(info os.FileInfo) bool { return false }
