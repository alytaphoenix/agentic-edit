//go:build unix

package tx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// rootLock is an advisory, cross-process lock keyed by the workspace root. It
// serialises aed transactions against each other; edits made by other tools are
// still detected by the content-hash guard.
type rootLock struct {
	f *os.File
}

func acquireRootLock(root string) (*rootLock, error) {
	base := os.TempDir()
	if cache, err := os.UserCacheDir(); err == nil {
		base = cache
	}
	dir := filepath.Join(base, "aed", "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// A lock is defence in depth. If the state dir is unavailable, continue
		// without it rather than making every edit fail.
		return nil, nil
	}
	sum := sha256.Sum256([]byte(root))
	name := filepath.Join(dir, hex.EncodeToString(sum[:])[:32]+".lock")
	// The name is a hash of the resolved root under a 0700 per-user cache dir;
	// O_NOFOLLOW prevents a pre-created symlink from redirecting the open.
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) // #nosec G304 -- generated path under a private dir
	if err != nil {
		return nil, nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another aed transaction is running for this workspace: %w", err)
	}
	return &rootLock{f: f}, nil
}

func (l *rootLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
