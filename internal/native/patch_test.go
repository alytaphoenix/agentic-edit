package native

import "testing"

func TestApplyPatchReplaceLine(t *testing.T) {
	patch := `--- a/f.txt
+++ b/f.txt
@@ -1,3 +1,3 @@
 a
-b
+B
 c
`
	out, err := ApplyPatch("a\nb\nc\n", patch)
	if err != nil {
		t.Fatal(err)
	}
	if out != "a\nB\nc\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestApplyPatchAddLine(t *testing.T) {
	patch := `@@ -1,1 +1,2 @@
 a
+b
`
	out, err := ApplyPatch("a\n", patch)
	if err != nil {
		t.Fatal(err)
	}
	if out != "a\nb\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestApplyPatchIntoEmptyFile(t *testing.T) {
	patch := `@@ -0,0 +1,2 @@
+a
+b
`
	out, err := ApplyPatch("", patch)
	if err != nil {
		t.Fatal(err)
	}
	if out != "a\nb\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestApplyPatchContextMismatch(t *testing.T) {
	patch := `@@ -1,2 +1,2 @@
 a
-b
+B
`
	if _, err := ApplyPatch("a\nc\n", patch); err == nil {
		t.Fatal("expected a context mismatch")
	}
}

func TestApplyPatchNoHunks(t *testing.T) {
	if _, err := ApplyPatch("a\n", "not a patch"); err == nil {
		t.Fatal("expected an error for a patch with no hunks")
	}
}
