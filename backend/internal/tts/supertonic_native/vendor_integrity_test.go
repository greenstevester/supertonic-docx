package supertonic_native

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// TestVendoredFileUnchanged catches accidental edits/corruption of the
// vendored copy. If you intentionally re-vendor, recompute helper.go.sha256
// (see Task 3 Step 1) and update VENDORED.md in the same commit.
func TestVendoredFileUnchanged(t *testing.T) {
	want, err := os.ReadFile("helper.go.sha256")
	if err != nil {
		t.Fatalf("read recorded sha: %v", err)
	}
	src, err := os.ReadFile("helper.go")
	if err != nil {
		t.Fatalf("read helper.go: %v", err)
	}
	sum := sha256.Sum256(src)
	got := hex.EncodeToString(sum[:])
	if got != strings.TrimSpace(string(want)) {
		t.Fatalf("helper.go sha256 mismatch:\n got  %s\n want %s\n(if intentional, update helper.go.sha256 + VENDORED.md)", got, strings.TrimSpace(string(want)))
	}
}

// TestLocalModificationsPresent asserts our two documented changes survive.
func TestLocalModificationsPresent(t *testing.T) {
	src, err := os.ReadFile("helper.go")
	if err != nil {
		t.Fatalf("read helper.go: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "package supertonic_native") {
		t.Error("package rename missing")
	}
	if !strings.Contains(s, "tts.SeedFunc()") {
		t.Error("seed hook missing from sampleNoisyLatent")
	}
}
