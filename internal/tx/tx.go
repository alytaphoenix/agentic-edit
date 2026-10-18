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
	"time"
)

// fileKind is the filesystem action a staged File performs.
type fileKind uint8

const (
	kindUpdate fileKind = iota
	kindCreate
	kindDelete
	kindRename
	kindChmod
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
	kind fileKind
	// newName is the destination for a rename, also root-relative.
	newName string
	// trashName is where a deleted file is parked until commit succeeds.
	trashName string
	// origMode is the mode restored by a chmod rollback.
	origMode os.FileMode
}

// Tx is a set of staged file updates that commit or roll back together.
type Tx struct {
	ID            string
	Root          string
	root          *os.Root
	dir           string
	files         []File
	applied       []File
	lock          *rootLock
	tmp           map[string]string // relative name -> staging temp path
	trash         map[string]string // original name -> parked deleted name
	keepTrash     bool              // rollback failed; keep parked copies for recovery
	needsRecovery bool              // rollback was incomplete; journal must remain
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
	return &Tx{ID: id, Root: root, root: r, dir: dir, lock: lk,
		tmp: map[string]string{}, trash: map[string]string{}}, nil
}

// rel returns path relative to the transaction root. It is lexical, but the
// root check in every filesystem operation rejects anything that escapes (for
// example through a symlinked parent).
func (t *Tx) rel(path string) (string, error) {
	// Resolve relative paths against the transaction root, never the process
	// working directory: plans and journals address files relative to the root.
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.Root, path)
	}
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
	t.files = append(t.files, File{Path: path, Before: before, After: after, Mode: mode, name: name, kind: kindUpdate})
	return nil
}

// StageCreate records a new file. The path must not already exist.
func (t *Tx) StageCreate(path string, after []byte, mode os.FileMode) error {
	name, err := t.rel(path)
	if err != nil {
		return err
	}
	if _, err := t.root.Lstat(name); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite it", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if mode == 0 {
		mode = 0o644
	}
	t.files = append(t.files, File{Path: path, After: after, Mode: mode, name: name, kind: kindCreate})
	return nil
}

// StageDelete records the removal of an existing regular file. before must be
// the exact content read during planning.
func (t *Tx) StageDelete(path string, before []byte, mode os.FileMode) error {
	name, err := t.rel(path)
	if err != nil {
		return err
	}
	info, err := t.root.Lstat(name)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || hardlinked(info) {
		return fmt.Errorf("%s is not a safe regular file to delete", path)
	}
	if mode == 0 {
		mode = info.Mode()
	}
	t.files = append(t.files, File{Path: path, Before: before, Mode: mode, name: name, kind: kindDelete})
	return nil
}

// StageRename records a rename from one root-relative path to another.
func (t *Tx) StageRename(from, to string, before []byte, mode os.FileMode) error {
	fromName, err := t.rel(from)
	if err != nil {
		return err
	}
	toName, err := t.rel(to)
	if err != nil {
		return err
	}
	info, err := t.root.Lstat(fromName)
	if err != nil {
		return fmt.Errorf("stat %s: %w", from, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || hardlinked(info) {
		return fmt.Errorf("%s is not a safe regular file to rename", from)
	}
	if _, err := t.root.Lstat(toName); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite it", to)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", to, err)
	}
	if mode == 0 {
		mode = info.Mode()
	}
	t.files = append(t.files, File{Path: from, Before: before, Mode: mode,
		name: fromName, newName: toName, kind: kindRename})
	return nil
}

// StageChmod records a mode change. Content is read so the transaction can
// detect a concurrent modification, and the original mode is kept for rollback.
func (t *Tx) StageChmod(path string, mode os.FileMode) error {
	name, err := t.rel(path)
	if err != nil {
		return err
	}
	info, err := t.root.Lstat(name)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || hardlinked(info) {
		return fmt.Errorf("%s is not a safe regular file to chmod", path)
	}
	cur, _, err := t.readRoot(name)
	if err != nil {
		return err
	}
	t.files = append(t.files, File{Path: path, Before: cur, Mode: mode,
		name: name, kind: kindChmod, origMode: info.Mode()})
	return nil
}

// NeedsRecovery reports whether a rollback was incomplete. Callers must leave
// the audit journal pending so `aed recover` can finish the job.
func (t *Tx) NeedsRecovery() bool { return t.needsRecovery }

// Files returns the staged (changed) files.
func (t *Tx) Files() []File { return append([]File(nil), t.files...) }

// Commit validates every target, stages new content, then applies each
// operation. It is all-or-nothing: any error restores every file already
// changed.
func (t *Tx) Commit() error {
	if len(t.files) == 0 {
		return nil
	}
	if t.root == nil {
		return fmt.Errorf("transaction is closed")
	}

	// 1. Preflight: validate targets and stage new content. Nothing visible has
	// changed yet, so a failure here just removes the staging files.
	for _, f := range t.files {
		switch f.kind {
		case kindCreate:
			if _, err := t.root.Lstat(f.name); err == nil {
				t.removeTemps()
				return fmt.Errorf("conflict: %s appeared since the plan was built", f.Path)
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.removeTemps()
				return fmt.Errorf("stat %s: %w", f.Path, err)
			}
			tmp, err := t.stageFile(f)
			if err != nil {
				t.removeTemps()
				return err
			}
			t.tmp[f.name] = tmp
		case kindUpdate:
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
		case kindDelete, kindChmod:
			if err := t.checkUnchanged(f); err != nil {
				t.removeTemps()
				return err
			}
		case kindRename:
			if err := t.checkUnchanged(f); err != nil {
				t.removeTemps()
				return err
			}
			if _, err := t.root.Lstat(f.newName); err == nil {
				t.removeTemps()
				return fmt.Errorf("conflict: %s appeared since the plan was built", f.newName)
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.removeTemps()
				return fmt.Errorf("stat %s: %w", f.newName, err)
			}
		default:
			t.removeTemps()
			return fmt.Errorf("unknown transaction operation")
		}
	}

	// 2. Apply. The hash is re-checked immediately before each change, which is
	// the narrowest window available without locking every file.
	for _, f := range t.files {
		var err error
		switch f.kind {
		case kindUpdate, kindCreate:
			if err = t.checkUnchangedOrAbsent(f); err != nil {
				break
			}
			err = t.root.Rename(t.tmp[f.name], f.name)
			if err == nil {
				delete(t.tmp, f.name)
			}
		case kindDelete:
			if err = t.checkUnchanged(f); err != nil {
				break
			}
			trash := filepath.Join(filepath.Dir(f.name), randomStagingName())
			if err = t.root.Rename(f.name, trash); err == nil {
				f.trashName = trash
				t.trash[f.name] = trash
			}
		case kindRename:
			if err = t.checkUnchanged(f); err != nil {
				break
			}
			err = t.root.Rename(f.name, f.newName)
		case kindChmod:
			if err = t.checkUnchanged(f); err != nil {
				break
			}
			err = t.chmod(f.name, f.Mode)
		}
		if err != nil {
			t.removeTemps()
			rbErr := t.Rollback()
			if rbErr != nil {
				t.needsRecovery = true
				return fmt.Errorf("%s: %w (rollback failed: %v)", f.Path, err, rbErr)
			}
			return fmt.Errorf("%s: %w (rolled back)", f.Path, err)
		}
		t.applied = append(t.applied, f)
		t.syncDir(f.name)
	}

	// 3. Verify the committed state, then discard parked deleted files.
	for _, f := range t.applied {
		var err error
		switch f.kind {
		case kindUpdate, kindCreate:
			err = t.verifyFile(f.name, f.After)
		case kindDelete:
			err = t.verifyAbsent(f.name)
		case kindRename:
			if err = t.verifyAbsent(f.name); err == nil {
				err = t.verifyExists(f.newName)
			}
		case kindChmod:
			err = t.verifyMode(f.name, f.Mode)
		}
		if err != nil {
			rbErr := t.Rollback()
			if rbErr != nil {
				t.needsRecovery = true
				return fmt.Errorf("verify %s: %w (rollback failed: %v)", f.Path, err, rbErr)
			}
			return fmt.Errorf("verify %s: %w (rolled back)", f.Path, err)
		}
	}
	return nil
}

// Rollback restores every file this transaction changed. It is safe to call
// after a successful Commit (used when a post-apply semantic check fails) and
// after a failed Commit.
func (t *Tx) Rollback() error {
	if t.root == nil {
		return fmt.Errorf("transaction is closed")
	}
	var errs []string
	for i := len(t.applied) - 1; i >= 0; i-- {
		f := t.applied[i]
		switch f.kind {
		case kindUpdate:
			// If another writer changed the file after our commit, restoring
			// our original would destroy their work. Refuse and report it.
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
		case kindCreate:
			cur, _, rerr := t.readRoot(f.name)
			if rerr != nil || Hash(cur) != Hash(f.After) {
				errs = append(errs, fmt.Sprintf("%s: changed since apply; refusing to remove it", f.Path))
				continue
			}
			if err := t.root.Remove(f.name); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
			}
		case kindDelete:
			if f.trashName == "" {
				errs = append(errs, fmt.Sprintf("%s: deleted file is no longer recoverable", f.Path))
				continue
			}
			if err := t.root.Rename(f.trashName, f.name); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
				continue
			}
			if err := t.verifyFile(f.name, f.Before); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
			}
		case kindRename:
			if _, err := t.root.Lstat(f.newName); err != nil {
				errs = append(errs, fmt.Sprintf("%s: renamed file is missing", f.Path))
				continue
			}
			if err := t.root.Rename(f.newName, f.name); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
			}
		case kindChmod:
			if err := t.chmod(f.name, f.origMode); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
			}
		}
	}
	if len(errs) > 0 {
		t.keepTrash = true
		t.needsRecovery = true
		return fmt.Errorf("rollback incomplete: %v", errs)
	}
	t.applied = nil
	t.trash = map[string]string{}
	return nil
}

// checkUnchangedOrAbsent applies the right existence check for a create/update.
func (t *Tx) checkUnchangedOrAbsent(f File) error {
	if f.kind == kindCreate {
		_, err := t.root.Lstat(f.name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("conflict: %s appeared since the plan was built", f.Path)
	}
	return t.checkUnchanged(f)
}

// chmod changes a file's mode through an opened descriptor (avoiding the
// path-race warning on os.Root.Chmod).
func (t *Tx) chmod(name string, mode os.FileMode) error {
	f, err := t.root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(mode)
}

func (t *Tx) verifyAbsent(name string) error {
	if _, err := t.root.Lstat(name); err == nil {
		return fmt.Errorf("%s still exists", name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (t *Tx) verifyExists(name string) error {
	info, err := t.root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	return nil
}

func (t *Tx) verifyMode(name string, want os.FileMode) error {
	info, err := t.root.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != want.Perm() {
		return fmt.Errorf("mode = %o, want %o", info.Mode().Perm(), want.Perm())
	}
	return nil
}

func (t *Tx) removeTrash() {
	if t.root == nil {
		return
	}
	for name, trash := range t.trash {
		_ = t.root.Remove(trash)
		delete(t.trash, name)
	}
	t.trash = map[string]string{}
}

// Cleanup removes staging files, releases the root lock, and closes the root.
// It does not change file contents.
func (t *Tx) Cleanup() {
	if t.root != nil {
		t.removeTemps()
		if !t.keepTrash {
			t.removeTrash()
		}
	}
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
	if t.root == nil {
		return
	}
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

// randomStagingName returns a random ".aed-..." basename.
func randomStagingName() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure means the process is in a bad state; use a
		// timestamp-derived fallback that is still unique enough for staging.
		return fmt.Sprintf(".aed-%d", time.Now().UnixNano())
	}
	return ".aed-" + hex.EncodeToString(b[:])
}

// stageFile writes content to a random, private temp file in the target's
// directory (through the root) and returns its root-relative path.
func (t *Tx) stageFile(f File) (string, error) {
	dir := filepath.Dir(f.name)
	for attempt := 0; attempt < 100; attempt++ {
		name := filepath.Join(dir, randomStagingName())
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
