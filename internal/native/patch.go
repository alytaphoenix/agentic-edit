package native

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ApplyPatch applies a unified diff (the format produced by diff -u and by most
// coding agents' apply_patch tools) to content. It is deliberately strict:
// every context and removed line must match exactly, hunk positions must be
// ordered, and line counts must agree. Any mismatch is an error, never a
// best-effort partial apply.
func ApplyPatch(content, patch string) (string, error) {
	doc := Parse(content)
	wasEmpty := len(doc.Lines) == 0
	patch = strings.ReplaceAll(patch, "\r\n", "\n")
	lines := strings.Split(patch, "\n")
	// A trailing newline in the patch is not an extra line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	type hunkLine struct {
		kind byte
		text string
	}
	type hunk struct {
		oldStart, oldCount int
		newStart, newCount int
		lines              []hunkLine
	}

	var hunks []hunk
	var cur *hunk
	seenHeader := false
	for _, raw := range lines {
		switch {
		case strings.HasPrefix(raw, "--- ") || strings.HasPrefix(raw, "+++ "):
			if cur != nil {
				hunks = append(hunks, *cur)
				cur = nil
			}
			seenHeader = true
			continue
		case strings.HasPrefix(raw, "@@"):
			if cur != nil {
				hunks = append(hunks, *cur)
			}
			m := hunkHeaderRe.FindStringSubmatch(raw)
			if m == nil {
				return "", fmt.Errorf("apply_patch: malformed hunk header %q", raw)
			}
			h := hunk{}
			h.oldStart = atoi(m[1])
			h.oldCount = 1
			if m[2] != "" {
				h.oldCount = atoi(m[2])
			}
			h.newStart = atoi(m[3])
			h.newCount = 1
			if m[4] != "" {
				h.newCount = atoi(m[4])
			}
			cur = &h
			continue
		}
		if cur == nil {
			// Ignore diff preamble lines (index, ---/+++ handled above).
			if strings.TrimSpace(raw) == "" || strings.HasPrefix(raw, "diff ") ||
				strings.HasPrefix(raw, "index ") || strings.HasPrefix(raw, "new file") ||
				strings.HasPrefix(raw, "deleted file") {
				continue
			}
			if seenHeader {
				continue
			}
			return "", fmt.Errorf("apply_patch: content before the first hunk")
		}
		if raw == "" {
			// Some patch producers omit the leading space on an empty context
			// line. Treat a bare empty line as context.
			cur.lines = append(cur.lines, hunkLine{kind: ' ', text: ""})
			continue
		}
		kind := raw[0]
		switch kind {
		case ' ', '-', '+':
			cur.lines = append(cur.lines, hunkLine{kind: kind, text: raw[1:]})
		case '\\':
			// "\ No newline at end of file" marker; the model is preserved by
			// the document, so ignore it.
		default:
			return "", fmt.Errorf("apply_patch: unexpected line %q", raw)
		}
	}
	if cur != nil {
		hunks = append(hunks, *cur)
	}
	if len(hunks) == 0 {
		return "", fmt.Errorf("apply_patch: no hunks found")
	}

	in := doc.Lines
	var out []string
	cursor := 0 // 0-based position in the original lines
	for _, h := range hunks {
		if h.oldCount < 0 || h.newCount < 0 {
			return "", fmt.Errorf("apply_patch: negative hunk count")
		}
		start := h.oldStart - 1
		if h.oldCount == 0 {
			// Insertion after line oldStart (0 means before line 1).
			start = h.oldStart
		}
		if start < cursor || start > len(in) {
			return "", fmt.Errorf("apply_patch: hunk starts at line %d, out of order or beyond the file", h.oldStart)
		}
		out = append(out, in[cursor:start]...)
		cursor = start

		oldSeen, newSeen := 0, 0
		for _, hl := range h.lines {
			switch hl.kind {
			case ' ':
				if cursor >= len(in) || in[cursor] != hl.text {
					return "", fmt.Errorf("apply_patch: context mismatch at line %d: have %q, want %q", cursor+1, lineAt(in, cursor), hl.text)
				}
				out = append(out, hl.text)
				cursor++
				oldSeen++
				newSeen++
			case '-':
				if cursor >= len(in) || in[cursor] != hl.text {
					return "", fmt.Errorf("apply_patch: removal mismatch at line %d: have %q, want %q", cursor+1, lineAt(in, cursor), hl.text)
				}
				cursor++
				oldSeen++
			case '+':
				out = append(out, hl.text)
				newSeen++
			}
		}
		if h.oldCount != 0 && oldSeen != h.oldCount {
			return "", fmt.Errorf("apply_patch: hunk at line %d removes %d line(s), header says %d", h.oldStart, oldSeen, h.oldCount)
		}
		if h.newCount != 0 && newSeen != h.newCount {
			return "", fmt.Errorf("apply_patch: hunk at line %d produces %d line(s), header says %d", h.newStart, newSeen, h.newCount)
		}
	}
	out = append(out, in[cursor:]...)
	doc.Lines = out
	if len(out) == 0 {
		doc.FinalNewline = false
	} else if wasEmpty {
		// A patch that creates content in an empty file conventionally ends
		// with a newline; the diff format cannot express the distinction.
		doc.FinalNewline = true
	}
	return doc.Render(), nil
}

func lineAt(lines []string, i int) string {
	if i < 0 || i >= len(lines) {
		return ""
	}
	return lines[i]
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
