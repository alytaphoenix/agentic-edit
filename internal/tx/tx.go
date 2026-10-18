// Package tx implements all-or-nothing, byte-exact file updates.
//
// Every write goes through a private staging file created with a random name
// (O_EXCL, 0600) inside the workspace root, is fsynced, and is then renamed over
// the target. All path access goes through os.Root, so a symlink — including one
// swapped in for a parent directory after the path was checked — can never
// redirect a read or write outside the root. Targets are re-hashed immediately
// before the rename, so a concurrent edit is detected instead of silently
// overwritten. If any step fails, every file already replaced is restored from
// its in-memory original.
//
// The package deliberately never creates sibling backup files such as
// "file.bak" or "file.aed.tmp": those names are predictable and were previously
// an arbitrary-write primitive when an attacker pre-created them as symlinks.
package tx

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// File is one staged update.
type File struct {
	Path   string
	Before []byte
	After  []byte
	Mode   os.FileMode

	// name is Path relative to the transaction root. All filesystem access in
	// Commit/Rollback goes through the root and this name.
	name string
}

// Tx is a set of staged file updates that commit or roll back together.
type Tx struct {
	ID      string
	Root    string
	root    *os.Root
	dir     string
	files   []File
	applied []File
	lock    *rootLock
	tmp     map[string]string // relative name -> staging temp path
}

// New creates a transaction rooted at root. The root is opened with os.Root, so
// every path accepted by Stage/Commit must resolve inside it. root also keys the
// cross-process advisory lock.
func New(root string) (*Tx, error) {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	dir, err := os.MkdirTemp("", "aed-tx-*")
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("create transaction dir: %w", err)
	}
	// 0700 on the private transaction directory, not a file.
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- private directory mode
		_ = r.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	id := filepath.Base(dir)
	lk, err := acquireRootLock(root)
	if err != nil {
		_ = r.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &Tx{ID: id, Root: root, root: r, dir: dir, lock: lk, tmp: map[string]string{}}, nil
}

// rel returns path relative to the transaction root. It is lexical, but the
// root check in every filesystem operation rejects anything that escapes (for
// example through a symlinked parent).
func (t *Tx) rel(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Resolve symlinks in the parent directory only. The final component is
	// deliberately left alone so Stage can reject a symlink target instead of
	// silently following it.
	if dir, base := filepath.Split(abs); dir != "" {
		if realDir, err := filepath.EvalSymlinks(dir); err == nil {
			abs = filepath.Join(realDir, base)
		}
	}
	rel, err := filepath.Rel(t.Root, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || filepath.IsAbs(rel) || hasDotDotPrefix(rel) {
		return "", fmt.Errorf("%s is outside the transaction root %s; refusing to edit it", path, t.Root)
	}
	return rel, nil
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(os.PathSeparator)
}

// Stage records an update. Unchanged files are ignored. A target that is a
// symlink, a hardlink, is outside the root, or is not a regular file is
// rejected before anything is written.
func (t *Tx) Stage(path string, before, after []byte, mode os.FileMode) error {
	if string(before) == string(after) {
		return nil
	}
	name, err := t.rel(path)
	if err != nil {
		return err
	}
	info, err := t.root.Lstat(name)
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
	t.files = append(t.files, File{Path: path, Before: before, After: after, Mode: mode, name: name})
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
	if t.root == nil {
		return fmt.Errorf("transaction is closed")
	}

	// 1. Validate all targets and write all staging files. Nothing visible has
	// changed yet, so a failure here just removes the staging files.
	for _, f := range t.files {
		if err := t.checkUnchanged(f); err != nil {
			t.removeTemps()
			return err
		}
		tmp, err := t.stageFile(f)
		if err != nil {
			t.removeTemps()
			return err
		}
		t.tmp[f.name] = tmp
	}

	// 2. Rename each staged file over its target. The hash is re-checked
	// immediately before each rename, which is the narrowest window we can
	// offer without holding a lock on every file. On failure, restore every
	// target already replaced.
	for _, f := range t.files {
		if err := t.checkUnchanged(f); err != nil {
			t.removeTemps()
			rbErr := t.Rollback()
			if rbErr != nil {
				return fmt.Errorf("%w (rollback failed: %v)", err, rbErr)
			}
			return err
		}
		tmp := t.tmp[f.name]
		if err := t.root.Rename(tmp, f.name); err != nil {
			t.removeTemps()
			rbErr := t.Rollback()
			if rbErr != nil {
				return fmt.Errorf("replace %s: %w (rollback failed: %v)", f.Path, err, rbErr)
			}
			return fmt.Errorf("replace %s: %w (rolled back)", f.Path, err)
		}
		delete(t.tmp, f.name)
		t.applied = append(t.applied, f)
		t.syncDir(f.name)
	}

	// 3. Verify the committed bytes. A mismatch is treated as a failed
	// transaction and restored.
	for _, f := range t.applied {
		if err := t.verifyFile(f.name, f.After); err != nil {
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
	if t.root == nil {
		return fmt.Errorf("transaction is closed")
	}
	var errs []string
	for i := len(t.applied) - 1; i >= 0; i-- {
		f := t.applied[i]
		// If another writer changed the file after our commit, restoring our
		// original would destroy their work. Refuse and report it instead.
		cur, _, rerr := t.readRoot(f.name)
		if rerr != nil || Hash(cur) != Hash(f.After) {
			errs = append(errs, fmt.Sprintf("%s: changed since apply; refusing to roll back", f.Path))
			continue
		}
		if err := t.atomicWrite(f.name, f.Before, f.Mode); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		if err := t.verifyFile(f.name, f.Before); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("rollback incomplete: %v", errs)
	}
	t.applied = nil
	return nil
}

// Cleanup removes staging files, releases the root lock, and closes the root.
// It does not change file contents.
func (t *Tx) Cleanup() {
	t.removeTemps()
	if t.lock != nil {
		t.lock.release()
		t.lock = nil
	}
	if t.dir != "" {
		_ = os.RemoveAll(t.dir)
		t.dir = ""
	}
	if t.root != nil {
		_ = t.root.Close()
		t.root = nil
	}
}

func (t *Tx) removeTemps() {
	for name, tmp := range t.tmp {
		_ = t.root.Remove(tmp)
		delete(t.tmp, name)
	}
}

// checkUnchanged re-reads the target through the root and reports a conflict if
// its bytes no longer match the expected original.
func (t *Tx) checkUnchanged(f File) error {
	info, err := t.root.Lstat(f.name)
	if err != nil {
		return fmt.Errorf("stat %s: %w", f.Path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s became a symlink; refusing to write", f.Path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", f.Path)
	}
	if hardlinked(info) {
		return fmt.Errorf("%s has multiple hard links; refusing to write", f.Path)
	}
	cur, _, err := t.readRoot(f.name)
	if err != nil {
		return fmt.Errorf("read %s: %w", f.Path, err)
	}
	if Hash(cur) != Hash(f.Before) {
		return fmt.Errorf("conflict: %s changed since it was read; nothing was written", f.Path)
	}
	return nil
}

// readRoot reads a file through the transaction root with O_NOFOLLOW, so a
// symlink swapped in for the final component cannot redirect the read.
func (t *Tx) readRoot(name string) ([]byte, os.FileMode, error) {
	f, err := openReadRoot(t.root, name)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("not a regular file")
	}
	data, err := io.ReadAll(f)
	return data, info.Mode(), err
}

// stageFile writes content to a random, private temp file in the target's
// directory (through the root) and returns its root-relative path.
func (t *Tx) stageFile(f File) (string, error) {
	dir := filepath.Dir(f.name)
	for attempt := 0; attempt < 100; attempt++ {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("generate staging name: %w", err)
		}
		name := filepath.Join(dir, ".aed-"+hex.EncodeToString(b[:]))
		tmp, err := t.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create staging file in %s: %w", filepath.Dir(f.Path), err)
		}
		cleanup := func() {
			_ = tmp.Close()
			_ = t.root.Remove(name)
		}
		if _, err := tmp.Write(f.After); err != nil {
			cleanup()
			return "", fmt.Errorf("write staging file: %w", err)
		}
		if err := tmp.Sync(); err != nil {
			cleanup()
			return "", fmt.Errorf("fsync staging file: %w", err)
		}
		if err := tmp.Chmod(f.Mode); err != nil {
			cleanup()
			return "", fmt.Errorf("chmod staging file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			_ = t.root.Remove(name)
			return "", fmt.Errorf("close staging file: %w", err)
		}
		return name, nil
	}
	return "", fmt.Errorf("could not create a unique staging file in %s", filepath.Dir(f.Path))
}

// atomicWrite replaces a root-relative path with content using a random staging
// file and rename.
func (t *Tx) atomicWrite(name string, content []byte, mode os.FileMode) error {
	f := File{Path: name, After: content, Mode: mode, name: name}
	tmp, err := t.stageFile(f)
	if err != nil {
		return err
	}
	if err := t.root.Rename(tmp, name); err != nil {
		_ = t.root.Remove(tmp)
		return err
	}
	t.syncDir(name)
	return nil
}

func (t *Tx) verifyFile(name string, want []byte) error {
	got, _, err := t.readRoot(name)
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

// syncDir fsyncs the directory containing a root-relative path so the new name
// is durable.
func (t *Tx) syncDir(name string) {
	d, err := t.root.Open(filepath.Dir(name))
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

// ReadFileLimit is ReadFile with a byte limit enforced before and during the
// read, so a huge or growing file cannot exhaust memory.
func ReadFileLimit(path string, max int64) ([]byte, os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("%s is a symlink; refusing to read it", path)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > max {
		return nil, 0, fmt.Errorf("%s exceeds max size %d bytes", path, max)
	}
	f, err := openRead(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > max {
		return nil, 0, fmt.Errorf("%s exceeds max size %d bytes", path, max)
	}
	return data, info.Mode(), nil
}

// ReadFull is like ReadFile but also returns a hash of the content.
func ReadFull(path string) ([]byte, os.FileMode, string, error) {
	data, mode, err := ReadFile(path)
	if err != nil {
		return nil, 0, "", err
	}
	return data, mode, Hash(data), nil
}
