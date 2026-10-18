// Package policy holds the shared path, protected-file, ignore, and
// blast-radius rules used by edit, plan/apply, and the MCP server. Keeping them
// in one place means every entry point enforces the same policy.
package policy

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bounds the blast radius of one plan. Zero fields mean "use the
// default" via Normalize.
type Limits struct {
	MaxFiles        int
	MaxFileBytes    int64
	MaxTotalBytes   int64
	MaxChangedLines int
	MaxDeletions    int
	MaxDiffBytes    int64
}

// DefaultLimits returns the conservative defaults used by the CLI and MCP.
func DefaultLimits() Limits {
	return Limits{
		MaxFiles:        32,
		MaxFileBytes:    50 * 1024 * 1024,
		MaxTotalBytes:   256 * 1024 * 1024,
		MaxChangedLines: 5000,
		MaxDeletions:    2500,
		MaxDiffBytes:    4 * 1024 * 1024,
	}
}

// Normalize fills zero fields with defaults.
func (l Limits) Normalize() Limits {
	d := DefaultLimits()
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxChangedLines <= 0 {
		l.MaxChangedLines = d.MaxChangedLines
	}
	if l.MaxDeletions <= 0 {
		l.MaxDeletions = d.MaxDeletions
	}
	if l.MaxDiffBytes <= 0 {
		l.MaxDiffBytes = d.MaxDiffBytes
	}
	return l
}

// DeniedError is a policy denial. Code is the process exit code (5 unless a
// caller maps it otherwise).
type DeniedError struct {
	Msg  string
	Code int
}

func (e *DeniedError) Error() string { return e.Msg }

// Deny returns a policy denial error.
func Deny(format string, args ...any) error {
	return &DeniedError{Msg: fmt.Sprintf(format, args...), Code: 5}
}

// IsDenied reports whether err is a policy denial.
func IsDenied(err error) bool {
	_, ok := err.(*DeniedError)
	return ok
}

// CheckLimits validates aggregate plan statistics.
func (l Limits) CheckLimits(files int, totalBytes int64, changedLines, deletions int, diffBytes int64) error {
	if files > l.MaxFiles {
		return Deny("%d files exceeds the limit of %d", files, l.MaxFiles)
	}
	if totalBytes > l.MaxTotalBytes {
		return Deny("planned content is %d bytes, over the %d-byte limit", totalBytes, l.MaxTotalBytes)
	}
	if changedLines > l.MaxChangedLines {
		return Deny("%d changed lines exceeds the limit of %d", changedLines, l.MaxChangedLines)
	}
	if deletions > l.MaxDeletions {
		return Deny("%d deleted lines exceeds the limit of %d", deletions, l.MaxDeletions)
	}
	if diffBytes > l.MaxDiffBytes {
		return Deny("diff is %d bytes, over the %d-byte limit", diffBytes, l.MaxDiffBytes)
	}
	return nil
}

// CheckText rejects binary and oversized file content.
func CheckText(name string, data []byte, max int64) error {
	if int64(len(data)) > max {
		return fmt.Errorf("%q exceeds max size %d bytes", name, max)
	}
	for _, b := range data {
		if b == 0 {
			return fmt.Errorf("%q looks like a binary file (contains a NUL byte)", name)
		}
	}
	return nil
}

// Confine resolves path against root and refuses anything outside it, whether
// via "..", an absolute path, or a symlink. It returns the resolved path and the
// path relative to the resolved root.
func Confine(root, p string) (realPath, rel string, err error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", err
	}
	// Refuse to edit a path whose final component is a symlink.
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("%q is a symlink; refusing to edit it", p)
	}
	realPath = abs
	if _, err := os.Stat(abs); err == nil {
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			realPath = r
		}
	} else if dir := filepath.Dir(abs); dir != "" {
		// The target does not exist yet (create/rename): resolve the parent so
		// a symlinked directory cannot smuggle it outside the root.
		if rd, err := filepath.EvalSymlinks(dir); err == nil {
			realPath = filepath.Join(rd, filepath.Base(abs))
		}
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", fmt.Errorf("workspace root %q is not accessible: %v", root, err)
	}
	rel, err = filepath.Rel(rootReal, realPath)
	if err != nil {
		return "", "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("%q is outside the workspace root %q", p, root)
	}
	return realPath, rel, nil
}

// IsProtected reports whether a root-relative path commonly leads to code
// execution or credential exposure. Matching is case-insensitive.
func IsProtected(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	relLower := strings.ToLower(rel)
	base := strings.ToLower(filepath.Base(rel))
	for _, p := range []string{
		".git/", ".hg/", ".svn/", ".ssh/", ".config/git/",
		".github/workflows/", ".github/actions/",
		".husky/", ".git/hooks/", ".gnupg/", ".kube/", ".docker/",
		".aws/", ".azure/", ".config/gcloud/", ".config/fish/",
	} {
		if relLower == strings.TrimSuffix(p, "/") || strings.HasPrefix(relLower, p) {
			return true
		}
	}
	for _, name := range []string{
		".env", ".envrc", ".npmrc", ".pypirc", ".netrc", ".bashrc", ".zshrc",
		".profile", ".bash_profile", ".zprofile", ".gitmodules",
		".gitconfig", "jenkinsfile", "makefile", "gnumakefile",
		"docker-compose.yml", "docker-compose.yaml", ".gitlab-ci.yml",
		".pre-commit-config.yaml", "lefthook.yml",
	} {
		if base == name {
			return true
		}
	}
	return strings.HasPrefix(base, ".env.") ||
		strings.HasPrefix(base, "id_rsa") ||
		strings.HasPrefix(base, "id_ed25519") ||
		strings.HasPrefix(base, "id_ecdsa") ||
		strings.HasPrefix(base, "credentials") ||
		strings.HasPrefix(base, "secrets") ||
		strings.HasPrefix(base, "dockerfile") ||
		strings.HasPrefix(base, "containerfile") ||
		strings.HasSuffix(base, ".pem") ||
		strings.HasSuffix(base, ".key") ||
		strings.HasSuffix(base, ".p12") ||
		strings.HasSuffix(base, ".pfx") ||
		strings.HasSuffix(base, ".jks") ||
		strings.HasSuffix(base, ".mk") ||
		strings.HasPrefix(base, "docker-compose.")
}

// Ignore is a parsed .aedignore file.
type Ignore struct {
	patterns []string
}

// LoadIgnore reads root/.aedignore when present. Blank lines and lines starting
// with "#" are ignored. Patterns use path.Match syntax against the root-relative
// path or its basename; a pattern ending in "/" matches that directory tree.
func LoadIgnore(root string) (*Ignore, error) {
	data, err := os.ReadFile(filepath.Join(root, ".aedignore")) // #nosec G304 -- fixed file under the workspace root
	if err != nil {
		if os.IsNotExist(err) {
			return &Ignore{}, nil
		}
		return nil, err
	}
	ig := &Ignore{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ig.patterns = append(ig.patterns, filepath.ToSlash(line))
	}
	return ig, nil
}

// Match reports whether rel is ignored.
func (i *Ignore) Match(rel string) bool {
	if i == nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	base := path.Base(rel)
	for _, p := range i.patterns {
		if strings.HasSuffix(p, "/") {
			if rel == strings.TrimSuffix(p, "/") || strings.HasPrefix(rel, p) {
				return true
			}
			continue
		}
		if p == rel || p == base {
			return true
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, base); ok {
			return true
		}
	}
	return false
}

// CheckPath applies protected/ignored policy to a relative path.
func CheckPath(rel string, allowProtected, allowIgnored bool, ig *Ignore) error {
	if !allowProtected && IsProtected(rel) {
		return Deny("%q is a protected path; re-run with --allow-protected to edit it", rel)
	}
	if !allowIgnored && ig.Match(rel) {
		return Deny("%q is listed in .aedignore; re-run with --allow-ignored to edit it", rel)
	}
	return nil
}
