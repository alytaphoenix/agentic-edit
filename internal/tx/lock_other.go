//go:build !unix

package tx

// rootLock is a no-op on platforms without flock. The content-hash guard still
// prevents overwriting a changed file.
type rootLock struct{}

func acquireRootLock(root string) (*rootLock, error) { return nil, nil }

func (l *rootLock) release() {}
