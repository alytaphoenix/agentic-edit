// Package audit maintains an append-only, hash-chained record of every applied
// transaction, plus the before-content journal that makes undo and crash
// recovery possible.
//
// The JSONL log lives under the user state directory (0700), never in the
// workspace. Each record commits to the previous record's hash, so removing or
// rewriting history is detectable with `audit.Verify`.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Version is the audit record format version.
const Version = 1

// Record is one audit-log entry.
type Record struct {
	Version     int         `json:"version"`
	Seq         int64       `json:"seq"`
	Time        string      `json:"time"`
	Kind        string      `json:"kind"` // apply | undo | recover | rollback | denied
	TXID        string      `json:"tx_id,omitempty"`
	Root        string      `json:"root"`
	Actor       string      `json:"actor,omitempty"`
	Tool        string      `json:"tool_version,omitempty"`
	Digest      string      `json:"plan_digest,omitempty"`
	Instruction string      `json:"instruction,omitempty"`
	Message     string      `json:"message,omitempty"`
	Files       []FileEntry `json:"files,omitempty"`
	PrevHash    string      `json:"prev_hash"`
	Hash        string      `json:"hash"`
}

// FileEntry describes one file's role in a transaction.
type FileEntry struct {
	Path      string `json:"path"`
	Action    string `json:"action"`
	BeforeSHA string `json:"before_sha256,omitempty"`
	AfterSHA  string `json:"after_sha256,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	OrigMode  uint32 `json:"orig_mode,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Blob      string `json:"blob,omitempty"`
}

var logMu sync.Mutex

// StateDir returns the per-user state directory, creating it with 0700.
func StateDir() (string, error) {
	base := ""
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		base = filepath.Join(home, ".local", "state", "aed")
	} else if cache, err := os.UserCacheDir(); err == nil {
		base = filepath.Join(cache, "aed", "state")
	} else {
		base = filepath.Join(os.TempDir(), "aed-state")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return base, nil
}

// LogPath returns the audit log path.
func LogPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "audit.jsonl"), nil
}

// TxnDir returns the journal root.
func TxnDir() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "tx")
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// Append adds a record, chaining it to the previous hash, and fsyncs the log.
func Append(rec Record) (Record, error) {
	logMu.Lock()
	defer logMu.Unlock()

	path, err := LogPath()
	if err != nil {
		return Record{}, err
	}
	prev, seq, err := last(path)
	if err != nil {
		return Record{}, err
	}
	rec.Version = Version
	rec.Seq = seq + 1
	if rec.Time == "" {
		rec.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	rec.PrevHash = prev
	rec.Hash = ""
	data, err := json.Marshal(rec)
	if err != nil {
		return Record{}, err
	}
	sum := sha256.Sum256(append([]byte(prev), data...))
	rec.Hash = hex.EncodeToString(sum[:])
	line, err := json.Marshal(rec)
	if err != nil {
		return Record{}, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- audit log under the private state dir
	if err != nil {
		return Record{}, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return Record{}, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return Record{}, err
	}
	if err := f.Close(); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// last returns the previous hash and sequence number.
func last(path string) (string, int64, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- audit log under the private state dir
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, nil
		}
		return "", 0, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			return "", 0, fmt.Errorf("audit log is corrupt at line %d: %w", i+1, err)
		}
		return rec.Hash, rec.Seq, nil
	}
	return "", 0, nil
}

// Read returns audit records in order. limit <= 0 means all.
func Read(limit int) ([]Record, error) {
	path, err := LogPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- audit log under the private state dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("audit log is corrupt: %w", err)
		}
		out = append(out, rec)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// Verify checks the sequence numbers and hash chain.
func Verify() error {
	recs, err := Read(0)
	if err != nil {
		return err
	}
	prev := ""
	for i, rec := range recs {
		if rec.Seq != int64(i+1) {
			return fmt.Errorf("audit record %d has sequence %d", i+1, rec.Seq)
		}
		if rec.PrevHash != prev {
			return fmt.Errorf("audit record %d has an invalid previous hash", i+1)
		}
		want := rec
		want.Hash = ""
		data, _ := json.Marshal(want)
		sum := sha256.Sum256(append([]byte(prev), data...))
		if hex.EncodeToString(sum[:]) != rec.Hash {
			return fmt.Errorf("audit record %d has an invalid hash", i+1)
		}
		prev = rec.Hash
	}
	return nil
}

// FindApply returns the most recent committed journal for root, optionally
// restricted to txID.
func FindApply(root, txID string) (*Journal, error) {
	metas, err := listJournals()
	if err != nil {
		return nil, err
	}
	for i := len(metas) - 1; i >= 0; i-- {
		m := metas[i]
		if m.Status != "committed" {
			continue
		}
		if root != "" && m.Root != root {
			continue
		}
		if txID != "" && m.TXID != txID {
			continue
		}
		return loadJournal(m)
	}
	return nil, fmt.Errorf("no undoable transaction found")
}
