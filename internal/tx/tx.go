// Package tx implements all-or-nothing, byte-exact file updates.
//
// Every write goes through a private staging file created with os.CreateTemp in
// the target directory (random name, O_EXCL, 0600), is fsynced, and is then
// renamed over the target. Targets are re-hashed immediately before the rename,
// so a concurrent edit is detected instead of silently overwritten. If any step
// fails, every file already replaced is restored from its in-memory original.
//
// The package deliberately never creates sibling backup files such as
// "file.bak" or "file.aed.tmp": those names are predictable and were previously
// an arbitrary-write primitive when an attacker pre-created them as symlinks.
package tx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// File is one staged update.
type File struct {
	Path   string
	Before []byte
	After  []byte
	Mode   os.FileMode
}

// Tx is a set of staged file updates that commit or roll back together.
type Tx struct {
	ID      string
	Root    string
	dir     string
	files   []File
	applied []File
	lock    *rootLock
	tmp     map[string]string // path -> staging temp path
}

// New creates a transaction rooted at root. root is only used to derive the
// cross-process lock; paths are confined by the caller.
func New(root string) (*Tx, error) {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	dir, err := os.MkdirTemp("", "aed-tx-*")
	if err != nil {
		return nil, fmt.Errorf("create transaction dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	id := filepath.Base(dir)
	lk, err := acquireRootLock(root)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &Tx{ID: id, Root: root, dir: dir, lock: lk, tmp: map[string]string{}}, nil
}

// Stage records an update. Unchanged files are ignored. A target that is a
// symlink, a hardlink, or not a regular file is rejected before anything is
// written.
func (t *Tx) Stage(path string, before, after []byte, mode os.FileMode) error {
	if string(before) == string(after) {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to edit it", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if hardlinked(info) {
		return fmt.Errorf("%s has multiple hard links; refusing to edit it", path)
	}
	if mode == 0 {
		mode = info.Mode()
	}
	t.files = append(t.files, File{Path: path, Before: before, After: after, Mode: mode})
	return nil
}

// Files returns the staged (changed) files.
func (t *Tx) Files() []File { return append([]File(nil), t.files...) }

// Commit validates every target against its expected original content, stages
// all new content, then renames every file into place. It is all-or-nothing:
// any error restores every file already replaced.
func (t *Tx) Commit() error {
	if len(t.files) == 0 {
		return nil
	}

	// 1. Validate all targets and write all staging files. Nothing visible has
	// changed yet, so a failure here just removes the staging files.
	for _, f := range t.files {
		if err := checkUnchanged(f.Path, f.Before); err != nil {
			t.removeTemps()
			return err
		}
		tmp, err := stageFile(f.Path, f.After, f.Mode)
		if err != nil {
			t.removeTemps()
			return err
		}
		t.tmp[f.Path] = tmp
	}

	// 2. Re-check hashes immediately before the first rename. This narrows the
	// window in which another process can change a target between our read and
	// our write.
	for _, f := range t.files {
		if err := checkUnchanged(f.Path, f.Before); err != nil {
			t.removeTemps()
			return err
		}
	}

	// 3. Rename each staged file over its target. On failure, restore every
	// target already replaced.
	for _, f := range t.files {
		tmp := t.tmp[f.Path]
		if err := os.Rename(tmp, f.Path); err != nil {
			t.removeTemps()
			rbErr := t.Rollback()
			if rbErr != nil {
				return fmt.Errorf("replace %s: %w (rollback failed: %v)", f.Path, err, rbErr)
			}
			return fmt.Errorf("replace %s: %w (rolled back)", f.Path, err)
		}
		delete(t.tmp, f.Path)
		t.applied = append(t.applied, f)
		syncDir(filepath.Dir(f.Path))
	}

	// 4. Verify the committed bytes. A mismatch is treated as a failed
	// transaction and restored.
	for _, f := range t.applied {
		if err := verifyFile(f.Path, f.After); err != nil {
			rbErr := t.Rollback()
			if rbErr != nil {
				return fmt.Errorf("verify %s: %w (rollback failed: %v)", f.Path, err, rbErr)
			}
			return fmt.Errorf("verify %s: %w (rolled back)", f.Path, err)
		}
	}
	return nil
}

// Rollback restores every file this transaction replaced. It is safe to call
// after a successful Commit (used when a post-apply semantic check fails) and
// after a failed Commit.
func (t *Tx) Rollback() error {
	var errs []string
	for i := len(t.applied) - 1; i >= 0; i-- {
		f := t.applied[i]
		// If another writer changed the file after our commit, restoring our
		// original would destroy their work. Refuse and report it instead.
		cur, _, rerr := ReadFile(f.Path)
		if rerr != nil || Hash(cur) != Hash(f.After) {
			errs = append(errs, fmt.Sprintf("%s: changed since apply; refusing to roll back", f.Path))
			continue
		}
		if err := atomicWrite(f.Path, f.Before, f.Mode); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		if err := verifyFile(f.Path, f.Before); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("rollback incomplete: %v", errs)
	}
	t.applied = nil
	return nil
}

// Cleanup removes staging files and releases the root lock. It does not change
// file contents.
func (t *Tx) Cleanup() {
	t.removeTemps()
	if t.lock != nil {
		t.lock.release()
		t.lock = nil
	}
	if t.dir != "" {
		os.RemoveAll(t.dir)
		t.dir = ""
	}
}

func (t *Tx) removeTemps() {
	for path, tmp := range t.tmp {
		os.Remove(tmp)
		delete(t.tmp, path)
	}
}

// checkUnchanged re-reads path and reports a conflict if its bytes no longer
// match the expected original.
func checkUnchanged(path string, before []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s became a symlink; refusing to write", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if hardlinked(info) {
		return fmt.Errorf("%s has multiple hard links; refusing to write", path)
	}
	cur, _, err := ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if Hash(cur) != Hash(before) {
		return fmt.Errorf("conflict: %s changed since it was read; nothing was written", path)
	}
	return nil
}

// stageFile writes content to a random, private temp file in the target's
// directory and returns its path. os.CreateTemp uses O_EXCL, so a pre-created
// path (including a symlink) cannot be used to redirect the write.
func stageFile(path string, content []byte, mode os.FileMode) (string, error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".aed-*")
	if err != nil {
		return "", fmt.Errorf("create staging file in %s: %w", dir, err)
	}
	name := f.Name()
	cleanup := func() {
		f.Close()
		os.Remove(name)
	}
	if _, err := f.Write(content); err != nil {
		cleanup()
		return "", fmt.Errorf("write staging file: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("fsync staging file: %w", err)
	}
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return "", fmt.Errorf("chmod staging file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("close staging file: %w", err)
	}
	return name, nil
}

// atomicWrite replaces path with content using a random staging file and rename.
func atomicWrite(path string, content []byte, mode os.FileMode) error {
	tmp, err := stageFile(path, content, mode)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

func verifyFile(path string, want []byte) error {
	got, _, err := ReadFile(path)
	if err != nil {
		return err
	}
	if Hash(got) != Hash(want) {
		return fmt.Errorf("content hash mismatch after write")
	}
	return nil
}

// Hash returns the hex SHA-256 of data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// syncDir fsyncs a directory after a rename so the new name is durable.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// ReadFile reads a file with an explicit regular-file check.
func ReadFile(path string) ([]byte, os.FileMode, error) {
	f, err := openRead(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(f)
	return data, info.Mode(), err
}

// ReadFull is like ReadFile but also returns a hash of the content.
func ReadFull(path string) ([]byte, os.FileMode, string, error) {
	data, mode, err := ReadFile(path)
	if err != nil {
		return nil, 0, "", err
	}
	return data, mode, Hash(data), nil
}
