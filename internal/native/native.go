// Package native applies typed edit operations directly to text in memory. It
// is the default engine for `aed edit` and the engine behind plan/apply.
//
// The engine is deliberately byte-stable for the parts of a file it does not
// edit: a UTF-8 BOM, the dominant line ending (LF or CRLF), and the presence or
// absence of a final newline are all preserved. That means the bytes that were
// previewed and syntax-checked are exactly the bytes the transaction writes.
//
// Operations are line-oriented like sed (a replace applies per line), but
// matching uses Go's RE2 syntax for regex operations. The old sed backend
// remains available with `--engine=sed` for compatibility.
package native

import (
	"fmt"
	"regexp"
	"strings"

	"agentic-edit/internal/exec"
)

// Document is parsed text with its encoding shape preserved.
type Document struct {
	BOM          bool
	EOL          string // "\n" or "\r\n"
	FinalNewline bool
	Lines        []string
}

// Parse splits content into lines and records BOM, line ending, and final
// newline. CRLF input is normalized to lines; Render restores the same shape.
func Parse(content string) Document {
	d := Document{EOL: "\n"}
	s := content
	if strings.HasPrefix(s, "\ufeff") {
		d.BOM = true
		s = strings.TrimPrefix(s, "\ufeff")
	}
	if strings.Contains(s, "\r\n") {
		d.EOL = "\r\n"
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	d.FinalNewline = strings.HasSuffix(s, "\n")
	if s == "" {
		return d
	}
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		// The input was exactly one line ending.
		d.Lines = []string{""}
		return d
	}
	d.Lines = strings.Split(s, "\n")
	return d
}

// Render joins the lines back into text with the original BOM, EOL, and final
// newline.
func (d Document) Render() string {
	body := strings.Join(d.Lines, d.EOL)
	if d.FinalNewline {
		body += d.EOL
	}
	if d.BOM {
		return "\ufeff" + body
	}
	return body
}

// Apply runs op against content and returns the new content. It never writes to
// disk and never shells out.
func Apply(op exec.EditOp, content string) (string, error) {
	if op.Kind == exec.KindGrep || op.Action == "grep" {
		return content, nil
	}
	doc := Parse(content)
	switch op.Action {
	case "transform":
		return applyTransform(op, doc)
	case "insert":
		return applyInsert(op, doc)
	case "delete":
		return applyDelete(op, doc)
	case "replace":
		return applyReplace(op, doc)
	case "":
		return applyLegacy(op, doc)
	default:
		return "", fmt.Errorf("native engine: unknown action %q", op.Action)
	}
}

func applyLegacy(op exec.EditOp, doc Document) (string, error) {
	switch op.Kind {
	case exec.KindAwk:
		return applyTransform(op, doc)
	case exec.KindSed:
		switch {
		case op.Old != "":
			return applyReplace(op, doc)
		case op.Pattern != "":
			return applyDelete(op, doc)
		case strings.Contains(op.Script, "\na\\") || strings.Contains(op.Script, "\ni\\"):
			return "", fmt.Errorf("native engine: legacy insert op has no anchor; use --engine=sed")
		}
	}
	return "", fmt.Errorf("native engine: cannot infer an action for %q op", op.Kind)
}

func applyTransform(op exec.EditOp, doc Document) (string, error) {
	switch op.Transform {
	case "upper":
		for i := range doc.Lines {
			doc.Lines[i] = strings.ToUpper(doc.Lines[i])
		}
	case "lower":
		for i := range doc.Lines {
			doc.Lines[i] = strings.ToLower(doc.Lines[i])
		}
	default:
		return "", fmt.Errorf("native engine: unknown transform %q", op.Transform)
	}
	return doc.Render(), nil
}

func applyReplace(op exec.EditOp, doc Document) (string, error) {
	if op.Old == "" {
		return "", fmt.Errorf("native engine: replace requires a non-empty old value")
	}
	if strings.ContainsAny(op.Old, "\r\n") || strings.ContainsAny(op.New, "\r\n") {
		return "", fmt.Errorf("native engine: replace values must be single-line")
	}

	regexMode := op.Extended
	var re *regexp.Regexp
	if regexMode {
		var err error
		re, err = regexp.Compile(op.Old)
		if err != nil {
			return "", fmt.Errorf("native engine: invalid regex %q: %w", op.Old, err)
		}
	}

	count := 0
	for _, line := range doc.Lines {
		if regexMode {
			count += len(re.FindAllStringIndex(line, -1))
		} else {
			count += strings.Count(line, op.Old)
		}
	}
	if op.ExpectCount > 0 && count != op.ExpectCount {
		return "", fmt.Errorf("expected %d occurrence(s) of %q but found %d; refusing to edit", op.ExpectCount, op.Old, count)
	}

	for i, line := range doc.Lines {
		if regexMode {
			doc.Lines[i] = replaceRegexLine(re, line, op.New, op.Global)
		} else if op.Global {
			doc.Lines[i] = strings.ReplaceAll(line, op.Old, op.New)
		} else {
			doc.Lines[i] = strings.Replace(line, op.Old, op.New, 1)
		}
	}
	return doc.Render(), nil
}

// replaceRegexLine replaces either all matches or the first match on a line.
// The replacement is literal: "$1" and "&" are not expanded, matching the
// typed-op contract where New is a value rather than a sed replacement string.
func replaceRegexLine(re *regexp.Regexp, line, newVal string, global bool) string {
	if global {
		idxs := re.FindAllStringIndex(line, -1)
		if len(idxs) == 0 {
			return line
		}
		var b strings.Builder
		last := 0
		for _, idx := range idxs {
			b.WriteString(line[last:idx[0]])
			b.WriteString(newVal)
			last = idx[1]
		}
		b.WriteString(line[last:])
		return b.String()
	}
	idx := re.FindStringIndex(line)
	if idx == nil {
		return line
	}
	return line[:idx[0]] + newVal + line[idx[1]:]
}

func applyInsert(op exec.EditOp, doc Document) (string, error) {
	if op.Anchor == "" {
		return "", fmt.Errorf("native engine: insert requires an anchor")
	}
	re, err := regexp.Compile(op.Anchor)
	if err != nil {
		return "", fmt.Errorf("native engine: invalid anchor %q: %w", op.Anchor, err)
	}
	text := strings.ReplaceAll(op.Text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	textLines := strings.Split(text, "\n")
	if len(textLines) == 0 {
		return "", fmt.Errorf("native engine: insert requires text")
	}
	var out []string
	inserted := false
	for _, line := range doc.Lines {
		match := re.MatchString(line) && !(op.FirstOnly && inserted)
		if match && op.InsertBefore {
			out = append(out, textLines...)
		}
		out = append(out, line)
		if match && !op.InsertBefore {
			out = append(out, textLines...)
		}
		if match {
			inserted = true
		}
	}
	doc.Lines = out
	return doc.Render(), nil
}

func applyDelete(op exec.EditOp, doc Document) (string, error) {
	if op.Pattern == "" {
		return "", fmt.Errorf("native engine: delete requires a pattern")
	}
	re, err := regexp.Compile(op.Pattern)
	if err != nil {
		return "", fmt.Errorf("native engine: invalid delete pattern %q: %w", op.Pattern, err)
	}
	out := make([]string, 0, len(doc.Lines))
	for _, line := range doc.Lines {
		if re.MatchString(line) {
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 && len(doc.Lines) > 0 && !op.DeleteAllAllowed {
		return "", fmt.Errorf("delete pattern %q removes every line; re-run with --force to allow whole-file deletion", op.Pattern)
	}
	doc.Lines = out
	if len(out) == 0 {
		doc.FinalNewline = false
	}
	return doc.Render(), nil
}
