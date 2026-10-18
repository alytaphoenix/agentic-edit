// Package scrub redacts secrets and credentials from text so they are never
// sent to an LLM or printed to output unless the caller explicitly opts out
// (--show-secrets). Redaction is output-only: it never changes what is written
// to disk.
package scrub

import (
	"fmt"
	"regexp"
)

// Marker is the format string substituted for each redacted secret.
const Marker = "[REDACTED:%s]"

// formatRe holds high-precision patterns for well-known secret formats. Each
// match is replaced with its named marker.
var formatRe = []struct {
	name string
	re   *regexp.Regexp
}{
	{
		name: "aws_access_key",
		re:   regexp.MustCompile(`(?:AKIA|ASIA|AGIA|AIDA)[0-9A-Z]{16}`),
	},
	{
		name: "private_key",
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	},
	{
		name: "github_token",
		re:   regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36}`),
	},
	{
		name: "slack_token",
		re:   regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`),
	},
	{
		name: "bearer_token",
		re:   regexp.MustCompile(`(?i)\bbearer\b[ \t]+[A-Za-z0-9._~+/=-]{20,}`),
	},
	{
		name: "connection_string",
		re:   regexp.MustCompile(`(?i)(?:postgres|postgresql|mysql|mongodb\+srv|mongodb|redis|amqp)://[^\s:@/]+:[^\s:@/]+@`),
	},
	{
		name: "openai_key",
		re:   regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_-]{16,}`),
	},
	{
		name: "gitlab_token",
		re:   regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),
	},
	{
		name: "google_api_key",
		re:   regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),
	},
	{
		name: "stripe_key",
		re:   regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{10,}`),
	},
	{
		name: "jwt",
		re:   regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	},
}

// quotedAssignRe redacts a quoted value assigned to a sensitive key, including
// values that contain spaces or punctuation. It runs before assignRe so the
// whole quoted value is replaced rather than just its first token.
var quotedAssignRe = regexp.MustCompile(`(?i)(\b(?:password|passwd|secret|api[_-]?key|apikey|access[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|credential|token)\b[^:=<> ]{0,40}\s*[:=]\s*["'])([^"'\n]{4,})(["'])`)

// assignRe is a heuristic that redacts the *value* assigned to a sensitive key.
// Group 1 (the key name and operator) is preserved; only the value is replaced.
// A minimum value length keeps ordinary identifiers (e.g. `token = "GET"`)
// from being scrubbed.
var assignRe = regexp.MustCompile(`(?i)(\b(?:password|passwd|secret|api[_-]?key|apikey|access[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|credential|token)\b[^:=<> ]{0,40}\s*[:=]\s*['\"]?)([A-Za-z0-9._~+/=-]{4,})`)

// Redact replaces known secrets in s with a redaction marker. It is idempotent:
// running it on already-redacted text is a no-op.
func Redact(s string) string {
	for _, f := range formatRe {
		s = f.re.ReplaceAllString(s, fmt.Sprintf(Marker, f.name))
	}
	s = quotedAssignRe.ReplaceAllString(s, "${1}"+fmt.Sprintf(Marker, "credential")+"${3}")
	s = assignRe.ReplaceAllString(s, "${1}"+fmt.Sprintf(Marker, "credential"))
	return s
}

// NoOp returns its input unchanged. Callers pass this when secret scrubbing is
// disabled (--show-secrets) so the rest of the code can call the scrub func
// unconditionally.
func NoOp(s string) string { return s }
