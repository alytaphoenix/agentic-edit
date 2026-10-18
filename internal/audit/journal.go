package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Meta is the on-disk journal record for one transaction.
type Meta struct {
	Version int         `json:"version"`
	TXID    string      `json:"tx_id"`
	Root    string      `json:"root"`
	Digest  string      `json:"plan_digest,omitempty"`
	Time    string      `json:"time"`
	Status  string      `json:"status"` // pending | committed | undone | recovered
	Files   []FileEntry `json:"files"`
}

// Journal is a transaction's before-content journal.
type Journal struct {
	Dir  string
	Meta Meta
}

// Begin writes a pending journal and its before-content blobs. It must be
// called before the transaction's first rename so a crash can be recovered.
func Begin(txID, root, digest string, files []FileEntry, before map[string][]byte) (*Journal, error) {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	base, err := TxnDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, txID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	meta := Meta{
		Version: Version, TXID: txID, Root: root, Digest: digest,
		Time: time.Now().UTC().Format(time.RFC3339Nano), Status: "pending",
	}
	for i := range files {
		data, ok := before[files[i].Path]
		if !ok {
			continue
		}
		name := fmt.Sprintf("%03d.before", i)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("write journal blob: %w", err)
		}
		files[i].Blob = name
	}
	meta.Files = files
	if err := writeMeta(dir, meta); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	prune()
	return &Journal{Dir: dir, Meta: meta}, nil
}

func writeMeta(dir string, meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "meta.json")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Commit marks the journal as committed (and therefore undoable).
func (j *Journal) Commit() error {
	j.Meta.Status = "committed"
	return writeMeta(j.Dir, j.Meta)
}

// Abort removes the journal after a failed/rolled-back transaction.
func (j *Journal) Abort() {
	_ = os.RemoveAll(j.Dir)
}

// BlobPath returns the absolute path of a file entry's before blob.
func (j *Journal) BlobPath(entry FileEntry) string {
	if entry.Blob == "" {
		return ""
	}
	return filepath.Join(j.Dir, entry.Blob)
}

func loadJournal(meta Meta) (*Journal, error) {
	base, err := TxnDir()
	if err != nil {
		return nil, err
	}
	return &Journal{Dir: filepath.Join(base, meta.TXID), Meta: meta}, nil
}

func listJournals() ([]Meta, error) {
	base, err := TxnDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var metas []Meta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, e.Name(), "meta.json")) // #nosec G304 -- journal under the private state dir
		if err != nil {
			continue
		}
		var m Meta
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		metas = append(metas, m)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Time < metas[j].Time })
	return metas, nil
}

// prune keeps the newest 50 journals, and never removes a pending one.
func prune() {
	metas, err := listJournals()
	if err != nil {
		return
	}
	base, err := TxnDir()
	if err != nil {
		return
	}
	kept := 0
	for i := len(metas) - 1; i >= 0; i-- {
		if metas[i].Status == "pending" {
			continue
		}
		kept++
		if kept > 50 {
			_ = os.RemoveAll(filepath.Join(base, metas[i].TXID))
		}
	}
}
