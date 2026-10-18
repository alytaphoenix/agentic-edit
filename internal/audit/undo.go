package audit

import (
	"fmt"
	"os"
	"path/filepath"

	"agentic-edit/internal/policy"
	"agentic-edit/internal/tx"
)

// UndoResult summarizes an undo or recovery pass.
type UndoResult struct {
	TXID  string   `json:"tx_id"`
	Files []string `json:"files"`
}

// Undo restores the most recent committed transaction for root, or a specific
// txID. Every file must still match the transaction's after-hash (or already
// match its before-hash); anything else is refused rather than overwritten.
func Undo(root, txID string) (*UndoResult, error) {
	realRoot, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	j, err := FindApply(realRoot, txID)
	if err != nil {
		return nil, err
	}
	tr, err := tx.New(realRoot)
	if err != nil {
		return nil, err
	}
	defer tr.Cleanup()

	var changed []string
	for _, e := range j.Meta.Files {
		before, err := readBlob(j, e)
		if err != nil {
			return nil, err
		}
		real, _, err := policy.Confine(realRoot, filepath.Join(realRoot, filepath.FromSlash(pathFor(e))))
		if err != nil {
			return nil, err
		}
		switch e.Action {
		case "update":
			cur, _, err := tx.ReadFile(real)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Path, err)
			}
			if tx.Hash(cur) == e.BeforeSHA {
				continue // already undone
			}
			if tx.Hash(cur) != e.AfterSHA {
				return nil, conflictf("%s changed since the transaction; refusing to undo", e.Path)
			}
			mode := e.OrigMode
			if mode == 0 {
				mode = e.Mode
			}
			if err := tr.Stage(e.Path, cur, before, os.FileMode(mode)); err != nil {
				return nil, err
			}
			changed = append(changed, e.Path)
		case "create":
			cur, _, err := tx.ReadFile(real)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			if tx.Hash(cur) != e.AfterSHA {
				return nil, conflictf("%s changed since the transaction; refusing to undo", e.Path)
			}
			if err := tr.StageDelete(e.Path, cur, os.FileMode(e.Mode)); err != nil {
				return nil, err
			}
			changed = append(changed, e.Path)
		case "delete":
			if _, _, err := tx.ReadFile(real); err == nil {
				return nil, conflictf("%s was recreated; refusing to undo over it", e.Path)
			}
			if err := tr.StageCreate(e.Path, before, os.FileMode(e.Mode)); err != nil {
				return nil, err
			}
			changed = append(changed, e.Path)
		case "rename":
			toReal, _, err := policy.Confine(realRoot, filepath.Join(realRoot, filepath.FromSlash(e.To)))
			if err != nil {
				return nil, err
			}
			if _, _, err := tx.ReadFile(real); err == nil {
				continue // already undone
			}
			cur, _, err := tx.ReadFile(toReal)
			if err != nil {
				return nil, conflictf("%s: renamed file is missing", e.To)
			}
			if tx.Hash(cur) != e.AfterSHA {
				return nil, conflictf("%s changed since the transaction; refusing to undo", e.To)
			}
			if err := tr.StageRename(e.To, e.From, cur, os.FileMode(e.Mode)); err != nil {
				return nil, err
			}
			changed = append(changed, e.From)
		case "chmod":
			cur, _, err := tx.ReadFile(real)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Path, err)
			}
			if e.AfterSHA != "" && tx.Hash(cur) != e.AfterSHA {
				return nil, conflictf("%s changed since the transaction; refusing to undo", e.Path)
			}
			if err := tr.StageChmod(e.Path, os.FileMode(e.OrigMode)); err != nil {
				return nil, err
			}
			changed = append(changed, e.Path)
		default:
			return nil, fmt.Errorf("unknown journal action %q", e.Action)
		}
	}
	if len(changed) == 0 {
		return &UndoResult{TXID: j.Meta.TXID, Files: changed}, nil
	}
	if err := tr.Commit(); err != nil {
		return nil, err
	}
	j.Meta.Status = "undone"
	_ = writeMeta(j.Dir, j.Meta)
	_, _ = Append(Record{
		Kind: "undo", TXID: tr.ID, Root: realRoot, Tool: "aed",
		Message: "undo " + j.Meta.TXID, Files: j.Meta.Files,
	})
	return &UndoResult{TXID: tr.ID, Files: changed}, nil
}

// Recover finishes or rolls back any journal left pending by a crash. A file
// that still matches the before-hash is left alone; one that matches the
// after-hash is restored; anything else is reported and that journal is skipped.
func Recover(root string) ([]UndoResult, error) {
	realRoot, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	metas, err := listJournals()
	if err != nil {
		return nil, err
	}
	var out []UndoResult
	for _, meta := range metas {
		if meta.Status != "pending" || meta.Root != realRoot {
			continue
		}
		j, err := loadJournal(meta)
		if err != nil {
			return nil, err
		}
		res, err := recoverOne(j, realRoot)
		if err != nil {
			return out, err
		}
		out = append(out, *res)
	}
	return out, nil
}

func recoverOne(j *Journal, root string) (*UndoResult, error) {
	tr, err := tx.New(root)
	if err != nil {
		return nil, err
	}
	defer tr.Cleanup()
	var changed []string
	for _, e := range j.Meta.Files {
		before, err := readBlob(j, e)
		if err != nil {
			return nil, err
		}
		real, _, err := policy.Confine(root, filepath.Join(root, filepath.FromSlash(pathFor(e))))
		if err != nil {
			return nil, err
		}
		switch e.Action {
		case "update":
			cur, _, err := tx.ReadFile(real)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Path, err)
			}
			switch tx.Hash(cur) {
			case e.BeforeSHA:
				continue
			case e.AfterSHA:
				mode := e.OrigMode
				if mode == 0 {
					mode = e.Mode
				}
				if err := tr.Stage(e.Path, cur, before, os.FileMode(mode)); err != nil {
					return nil, err
				}
				changed = append(changed, e.Path)
			default:
				return nil, conflictf("%s changed during recovery; refusing to write", e.Path)
			}
		case "create":
			cur, _, err := tx.ReadFile(real)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			if tx.Hash(cur) != e.AfterSHA {
				return nil, conflictf("%s changed during recovery; refusing to remove it", e.Path)
			}
			if err := tr.StageDelete(e.Path, cur, os.FileMode(e.Mode)); err != nil {
				return nil, err
			}
			changed = append(changed, e.Path)
		case "delete":
			if _, _, err := tx.ReadFile(real); err == nil {
				continue
			}
			if err := tr.StageCreate(e.Path, before, os.FileMode(e.Mode)); err != nil {
				return nil, err
			}
			changed = append(changed, e.Path)
		case "rename":
			toReal, _, err := policy.Confine(root, filepath.Join(root, filepath.FromSlash(e.To)))
			if err != nil {
				return nil, err
			}
			if _, _, err := tx.ReadFile(real); err == nil {
				continue
			}
			cur, _, err := tx.ReadFile(toReal)
			if err != nil {
				continue
			}
			if tx.Hash(cur) != e.AfterSHA {
				return nil, conflictf("%s changed during recovery; refusing to rename it back", e.To)
			}
			if err := tr.StageRename(e.To, e.From, cur, os.FileMode(e.Mode)); err != nil {
				return nil, err
			}
			changed = append(changed, e.From)
		case "chmod":
			if info, err := os.Stat(real); err == nil && info.Mode().Perm() != os.FileMode(e.OrigMode).Perm() {
				if err := tr.StageChmod(e.Path, os.FileMode(e.OrigMode)); err != nil {
					return nil, err
				}
				changed = append(changed, e.Path)
			}
		}
	}
	if len(changed) > 0 {
		if err := tr.Commit(); err != nil {
			return nil, err
		}
	}
	j.Meta.Status = "recovered"
	_ = writeMeta(j.Dir, j.Meta)
	_, _ = Append(Record{Kind: "recover", TXID: tr.ID, Root: root, Tool: "aed", Files: j.Meta.Files})
	return &UndoResult{TXID: tr.ID, Files: changed}, nil
}

func pathFor(e FileEntry) string {
	if e.Action == "rename" && e.From != "" {
		return e.From
	}
	return e.Path
}

func readBlob(j *Journal, e FileEntry) ([]byte, error) {
	p := j.BlobPath(e)
	if p == "" {
		return nil, fmt.Errorf("%s: no before-content blob", e.Path)
	}
	return os.ReadFile(p) // #nosec G304 -- before-blob inside the private state dir
}

func resolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}
