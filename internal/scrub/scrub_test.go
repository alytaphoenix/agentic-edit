package scrub

import "testing"

func TestRedactFormatPatterns(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string // a secret substring that must NOT survive redaction
	}{
		{"aws", `key = "AKIA1234567890ABCDEF"`, "AKIA1234567890ABCDEF"},
		{"private_key_block", "-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----", "MIIB"},
		{"github", `token=ghp_1234567890abcdefghijklmnopqrstuvwxyz`, "ghp_1234567890abcdefghijklmnopqrstuvwxyz"},
		{"slack", `xoxb-1234567890-abcdefghij`, "xoxb-1234567890-abcdefghij"},
		{"connection_string", "postgres://admin:s3cr3t@db.example.com:5432/app", "s3cr3t"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			if got == c.in {
				t.Fatalf("input was not redacted: %q", c.in)
			}
			if contains(got, c.secret) {
				t.Errorf("secret still present after redaction: %q", got)
			}
			if !contains(got, "[REDACTED") {
				t.Errorf("expected a redaction marker in %q", got)
			}
		})
	}
}

func TestRedactAssignment(t *testing.T) {
	got := Redact(`api_key: abcdef1234567890`)
	if contains(got, "abcdef1234567890") {
		t.Errorf("secret value survived: %q", got)
	}
	// The key name should be preserved.
	if !contains(got, "api_key") {
		t.Errorf("key name should be preserved, got %q", got)
	}
}

// TestNoFalsePositiveOnNormalCode is a required Phase 4 test: ordinary source
// — imports, hashes, UUIDs, base64, short token values, prose "bearer" — must
// pass through untouched.
func TestNoFalsePositiveOnNormalCode(t *testing.T) {
	normal := []string{
		`import "github.com/user/repo/path"`,
		`550e8400-e29b-41d4-a716-446655440000`,
		`d41d8cd98f00b204e9800998ecf8427e`,
		`aGVsbG8rZm9vYmFyMTIzNDU2Nzg5MA==`,
		`token = "GET"`,
		`secret = "x"`,
		`// The courier is the bearer of the ring.`,
		`const A = "v1"`,
		`accessControl = "read"`,
		`url = "https://example.com/docs"`,
	}
	for _, s := range normal {
		if got := Redact(s); got != s {
			t.Errorf("Redact changed normal text:\n  in:  %s\n  out: %s", s, got)
		}
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	secrets := []string{
		`key = "AKIA1234567890ABCDEF"`,
		`api_key: abcdef1234567890`,
		"-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----",
	}
	for _, s := range secrets {
		once := Redact(s)
		twice := Redact(once)
		if once != twice {
			t.Errorf("Redact not idempotent on %q:\n once: %s\n twice: %s", s, once, twice)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
