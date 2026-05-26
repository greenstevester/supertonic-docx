# Real Supertonic 3 ONNX Inference Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the stubbed `synthesizeOne` (returns silence) with real Supertonic 3 inference by vendoring upstream's Go engine, keeping the single-binary design and the existing `tts.Engine` interface unchanged.

**Architecture:** Vendor `github.com/supertone-inc/supertonic` `go/helper.go` (MIT) into `backend/internal/tts/supertonic_native/`, add one seed hook for reproducible evals, and rewire `tts.Engine` to drive it. The 4-model ONNX pipeline (`text_encoder → duration_predictor → vector_estimator → vocoder`) runs in-process via `github.com/yalue/onnxruntime_go`, which dlopens the ONNX Runtime C library bundled into the Docker image. A shared `audiocheck` package backs both a runtime silence-guard and a standalone Tier-0 audio scanner; Tier-1 (seed-pinned parity) and Tier-2 (Whisper WER/CER) are separate, user-run eval tools.

**Tech Stack:** Go 1.22, Gin, `yalue/onnxruntime_go` (purego/dlopen), `go-audio/{wav,audio}`, `mjibson/go-dsp`, `golang.org/x/text`; ONNX Runtime v1.16.0 (aarch64) native lib; Docker (linux/arm64); Whisper (user-supplied) for Tier 2.

---

## Verification reality (read first)

The dev/authoring environment **has no model assets and no ONNX Runtime native library**, so inference **cannot be run here**. That splits verification in two:

- **Offline (CI / authoring env):** `go build ./...`, `go vet ./...`, `gofmt -l .` (must print nothing), and all **non-model** unit tests. These are the gate for every code task below.
- **Model-dependent (USER runs):** every eval tier (Tier 0 runtime behaviour, Tier 1 parity, Tier 2 WER), the Docker image build/run, and any "it actually speaks" claim. These ship as scripts/tagged-tests with exact commands and expected output for the user to run. **Never claim inference works without a real run.**

When a step says **(USER-RUN)** it is not expected to pass in the authoring env; compile-checking it (`go build`/`go vet`, or `go build -tags model_evals`) is the offline obligation.

---

## File structure

**Create:**
- `backend/internal/tts/supertonic_native/helper.go` — vendored upstream engine (package rename + seed hook).
- `backend/internal/tts/supertonic_native/LICENSE` — upstream MIT license (attribution).
- `backend/internal/tts/supertonic_native/VENDORED.md` — provenance: source repo, pinned commit, modifications, checksums.
- `backend/internal/tts/supertonic_native/helper.go.sha256` — recorded sha256 of the vendored file (drift guard).
- `backend/internal/tts/supertonic_native/vendor_integrity_test.go` — offline drift/corruption test.
- `backend/internal/audiocheck/audiocheck.go` — shared audio sanity checks + canonical-WAV reader.
- `backend/internal/audiocheck/audiocheck_test.go` — offline unit tests.
- `backend/internal/tts/parity_model_test.go` — Tier 1 parity test, build-tagged `model_evals` (USER-RUN).
- `backend/eval/audiocheck/main.go` — Tier 0 standalone outbox scanner (USER-RUN).
- `backend/eval/wer/main.go` — Tier 2 driver: synthesize → ASR → WER/CER (USER-RUN).
- `backend/eval/wer/metrics.go` — WER/CER/normalize (pure, offline-testable).
- `backend/eval/wer/metrics_test.go` — offline unit tests for the metrics.
- `backend/eval/wer/corpus.json` — fixed eval set (en+de+ja × ≥2 voices + edge texts).
- `backend/eval/wer/thresholds.json` — per-language pass thresholds.
- `backend/eval/asr.sh.example` — sample Whisper wrapper the user copies to `asr.sh`.
- `backend/eval/README.md` — how to run all three tiers.
- `scripts/verify-vendor.sh` — network integrity check against the pinned upstream commit.
- `.dockerignore` — keep `assets/`, `outbox/`, `.git`, etc. out of the build context.

**Modify:**
- `backend/internal/tts/supertonic.go` — `Engine` rewrite (real inference; interface unchanged).
- `backend/internal/tts/languages.go` — drive languages off vendored `AvailableLangs`.
- `backend/internal/tts/supertonic_test.go` — **create** (offline tests for `discoverVoices`, `encodeWAV`).
- `backend/internal/tts/languages_test.go` — **create** (offline tests for language wiring).
- `backend/internal/api/server.go` — dedupe the language-name table to `tts.LanguageName`.
- `backend/go.mod`, `backend/go.sum` — add upstream deps via `go mod tidy`.
- `Dockerfile` — bundle the ONNX Runtime native lib; set `ONNXRUNTIME_LIB_PATH`.
- `docker-compose.yml` — pin `platform: linux/arm64`.
- `scripts/fetch-model.sh` — post-clone sanity check for the real asset layout.
- `README.md`, `CLAUDE.md` — remove "stubbed/silence" warnings; document native dep, asset layout, evals.

**Untouched (interface preserved):** `internal/api/jobs.go`, `internal/audio/*`, `internal/docx/*`, `cmd/server/*`. The whole point of the seam is that these need no edits.

---

## Task 1: Vendor the upstream engine

**Files:**
- Create: `backend/internal/tts/supertonic_native/helper.go`
- Create: `backend/internal/tts/supertonic_native/LICENSE`
- Create: `backend/internal/tts/supertonic_native/VENDORED.md`
- Modify: `backend/go.mod`, `backend/go.sum`

This task is a copy + dependency-add, not TDD. Its verification is "the vendored package compiles."

- [ ] **Step 1: Clone upstream at a pinned commit and record it**

```bash
git clone https://github.com/supertone-inc/supertonic /tmp/supertonic
git -C /tmp/supertonic rev-parse HEAD        # COPY THIS SHA — it goes in VENDORED.md
shasum -a 256 /tmp/supertonic/go/helper.go   # COPY pristine sha — goes in VENDORED.md
ls /tmp/supertonic/go/*.go                    # confirm the file set
```

Expected: a commit SHA, a sha256, and a listing that shows `helper.go` and `example_onnx.go` (and possibly `*_test.go`). Only `helper.go` carries the engine; `example_onnx.go` is the CLI (excluded).

- [ ] **Step 2: Copy the engine file and LICENSE, exclude the CLI**

```bash
mkdir -p backend/internal/tts/supertonic_native
# Vendor EVERY upstream go/*.go EXCEPT example_onnx.go and *_test.go.
# In practice that is just helper.go; if `ls` in Step 1 showed others, copy them too.
cp /tmp/supertonic/go/helper.go backend/internal/tts/supertonic_native/helper.go
cp /tmp/supertonic/LICENSE       backend/internal/tts/supertonic_native/LICENSE
```

- [ ] **Step 3: Rename the package**

Change the package clause at the top of `backend/internal/tts/supertonic_native/helper.go`:

```go
// before
package main

// after
package supertonic_native
```

(If you copied additional `.go` files, change `package main` → `package supertonic_native` in each.)

- [ ] **Step 4: Add upstream dependencies and tidy**

`helper.go` imports `ort "github.com/yalue/onnxruntime_go"`, `github.com/go-audio/wav`, `github.com/go-audio/audio`, `github.com/mjibson/go-dsp/...`, and `golang.org/x/text/unicode/norm`. Resolve them:

```bash
cd backend
go mod tidy
```

Then confirm the binding version matches upstream (the design records **v1.11.0**; use whatever `/tmp/supertonic/go/go.mod` pins if it differs, and pin it exactly per our "pin all deps" rule):

```bash
grep onnxruntime_go go.mod        # expect: github.com/yalue/onnxruntime_go v1.11.0
grep -E 'go-audio|go-dsp' go.mod  # expect these promoted to direct requires
```

- [ ] **Step 5: Verify the vendored package compiles, vets, and is formatted**

```bash
cd backend
go build ./internal/tts/supertonic_native/
go vet ./internal/tts/supertonic_native/
gofmt -l internal/tts/supertonic_native/
```

Expected: `go build`/`go vet` succeed with no output; `gofmt -l` prints nothing. `onnxruntime_go` compiles without the native `.so` present (it dlopens at runtime), so this passes offline.

- [ ] **Step 6: Write VENDORED.md**

Create `backend/internal/tts/supertonic_native/VENDORED.md`:

```markdown
# Vendored: supertonic Go engine

- **Source:** https://github.com/supertone-inc/supertonic — `go/helper.go`
- **Commit:** <SHA from Task 1 Step 1>
- **License:** MIT (see ./LICENSE, copied verbatim from the upstream repo root;
  confirmed 2026-05-26). Vendoring permitted with the copyright notice + license text.
- **Pristine helper.go sha256:** <sha from Task 1 Step 1>
- **Vendored helper.go sha256:** <filled in Task 3>

## Local modifications (and only these)

1. Package clause: `package main` → `package supertonic_native`.
2. Seed hook: `sampleNoisyLatent` reseeds from `tts.SeedFunc()` instead of a
   hardcoded `time.Now().UnixNano()`; `TextToSpeech.SeedFunc` is added and
   defaults to time-based in `LoadTextToSpeech` (see Task 2). Production
   behaviour is unchanged; evals pin the seed for reproducibility.

No other lines differ from upstream. `scripts/verify-vendor.sh` enforces this.

## ONNX Runtime native library

Inference needs the ONNX Runtime C library at runtime, located via
`ONNXRUNTIME_LIB_PATH` (default fallback `/usr/local/lib/libonnxruntime.so`).
The Docker image bundles v1.16.0 aarch64. `InitializeONNXRuntime()` (in this
package) performs `ort.SetSharedLibraryPath` + `ort.InitializeEnvironment`.
```

- [ ] **Step 7: Commit**

```bash
git add backend/internal/tts/supertonic_native/ backend/go.mod backend/go.sum
git commit -m "feat: vendor supertonic Go inference engine (MIT)"
```

---

## Task 2: Add the seed hook to the vendored engine

**Files:**
- Modify: `backend/internal/tts/supertonic_native/helper.go`

Upstream `sampleNoisyLatent` seeds Gaussian noise with `time.Now().UnixNano()`, so output is non-deterministic. We keep that default but make it injectable, which Tier 1 requires.

- [ ] **Step 1: Add the `SeedFunc` field to `TextToSpeech`**

In `helper.go`, add a field to the `TextToSpeech` struct (place it after `SampleRate`):

```go
type TextToSpeech struct {
	cfg           Config
	textProcessor *UnicodeProcessor
	dpOrt         *ort.DynamicAdvancedSession
	textEncOrt    *ort.DynamicAdvancedSession
	vectorEstOrt  *ort.DynamicAdvancedSession
	vocoderOrt    *ort.DynamicAdvancedSession
	SampleRate    int
	// SeedFunc supplies the RNG seed for sampleNoisyLatent. Defaults to a
	// time-based seed (set in LoadTextToSpeech) so production output stays
	// naturally varied; evals override it for reproducibility (LOCAL MOD).
	SeedFunc      func() int64
	baseChunkSize int
	chunkCompress int
	ldim          int
}
```

- [ ] **Step 2: Default `SeedFunc` in `LoadTextToSpeech`**

In `LoadTextToSpeech`, where the `&TextToSpeech{...}` literal is constructed (the one that sets `SampleRate: cfg.AE.SampleRate`), add the default:

```go
	textToSpeech := &TextToSpeech{
		// ...existing fields...
		SampleRate: cfg.AE.SampleRate,
		SeedFunc:   func() int64 { return time.Now().UnixNano() }, // LOCAL MOD
		// ...existing fields...
	}
```

(`time` is already imported by `helper.go`.)

- [ ] **Step 3: Use `SeedFunc` in `sampleNoisyLatent`**

Replace the single hardcoded seed line inside `sampleNoisyLatent`:

```go
	// before
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// after  (LOCAL MOD: injectable seed for reproducible evals)
	rng := rand.New(rand.NewSource(tts.SeedFunc()))
```

- [ ] **Step 4: Verify it still compiles, vets, formats**

```bash
cd backend
go build ./internal/tts/supertonic_native/
go vet ./internal/tts/supertonic_native/
gofmt -l internal/tts/supertonic_native/
```

Expected: all clean.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/tts/supertonic_native/helper.go
git commit -m "feat: add injectable seed hook to vendored sampleNoisyLatent"
```

---

## Task 3: Vendoring-integrity guard (offline test + network script)

**Files:**
- Create: `backend/internal/tts/supertonic_native/helper.go.sha256`
- Create: `backend/internal/tts/supertonic_native/vendor_integrity_test.go`
- Create: `scripts/verify-vendor.sh`
- Modify: `backend/internal/tts/supertonic_native/VENDORED.md`

Two layers: a cheap **offline** test that catches accidental corruption/drift of the *copy*, and a **network** script that proves the copy equals upstream-at-pinned-commit modulo the documented diff.

- [ ] **Step 1: Record the vendored file's sha256**

```bash
cd backend/internal/tts/supertonic_native
shasum -a 256 helper.go | awk '{print $1}' > helper.go.sha256
cat helper.go.sha256   # 64 hex chars
```

Paste the same value into `VENDORED.md` under "Vendored helper.go sha256".

- [ ] **Step 2: Write the offline integrity test**

Create `backend/internal/tts/supertonic_native/vendor_integrity_test.go`:

```go
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
```

- [ ] **Step 3: Run the offline test (must pass in authoring env)**

```bash
cd backend
go test ./internal/tts/supertonic_native/ -run 'TestVendored|TestLocalModifications' -v
```

Expected: `PASS` for both tests.

- [ ] **Step 4: Write the network integrity script**

Create `scripts/verify-vendor.sh` (executable):

```bash
#!/usr/bin/env bash
# verify-vendor.sh — prove the vendored engine equals upstream at the pinned
# commit, modulo our two documented changes (package rename + seed hook).
# Requires network + git. CI/maintainer tool, not part of `go test`.
set -euo pipefail

NATIVE_DIR="backend/internal/tts/supertonic_native"
VENDORED="$NATIVE_DIR/helper.go"
COMMIT="$(grep -oE '^- \*\*Commit:\*\* .+' "$NATIVE_DIR/VENDORED.md" | awk '{print $3}')"

if [ -z "$COMMIT" ]; then echo "could not read pinned commit from VENDORED.md"; exit 1; fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
git clone --quiet https://github.com/supertone-inc/supertonic "$TMP/up"
git -C "$TMP/up" checkout --quiet "$COMMIT"

# Reproduce our edits on the pristine file, then diff.
sed 's/^package main$/package supertonic_native/' "$TMP/up/go/helper.go" \
  | sed 's/rand\.NewSource(time\.Now()\.UnixNano())/rand.NewSource(tts.SeedFunc())/' \
  > "$TMP/expected_partial.go"

echo "NOTE: the SeedFunc struct field + LoadTextToSpeech default are additive lines;"
echo "this script reproduces the sampleNoisyLatent change + package rename only."
echo "Reviewing the remaining diff confirms it is exactly those two additive edits:"
diff -u "$TMP/expected_partial.go" "$VENDORED" || true

echo
echo "Pristine upstream sha256:"
shasum -a 256 "$TMP/up/go/helper.go"
echo "(Compare against 'Pristine helper.go sha256' in VENDORED.md.)"
```

```bash
chmod +x scripts/verify-vendor.sh
```

- [ ] **Step 5: Smoke-run the script (USER-RUN — needs network)**

```bash
./scripts/verify-vendor.sh
```

Expected: prints the pristine sha (matching VENDORED.md) and a diff containing only the additive `SeedFunc` field + the `LoadTextToSpeech` default assignment (the `sed` already accounts for the package rename and the `sampleNoisyLatent` line).

- [ ] **Step 6: Commit**

```bash
git add backend/internal/tts/supertonic_native/helper.go.sha256 \
        backend/internal/tts/supertonic_native/vendor_integrity_test.go \
        backend/internal/tts/supertonic_native/VENDORED.md \
        scripts/verify-vendor.sh
git commit -m "test: add vendoring integrity guard (offline sha + network diff)"
```

---

## Task 4: `audiocheck` package (shared sanity checks + WAV reader)

**Files:**
- Create: `backend/internal/audiocheck/audiocheck.go`
- Test: `backend/internal/audiocheck/audiocheck_test.go`

This backs both the runtime guard (Task 5) and the Tier-0 tool (Task 8). Fully offline + TDD.

- [ ] **Step 1: Write the failing tests**

Create `backend/internal/audiocheck/audiocheck_test.go`:

```go
package audiocheck

import (
	"math"
	"testing"
)

func sine(n int, amp float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*440*float64(i)/24000))
	}
	return out
}

func TestGuard(t *testing.T) {
	tests := []struct {
		name    string
		samples []float32
		wantErr bool
	}{
		{"normal speech-like signal", sine(24000, 0.3), false},
		{"empty", nil, true},
		{"silent", make([]float32, 24000), true},
		{"near-silent below floor", sine(24000, 1e-6), true},
		{"all clipped", func() []float32 { s := make([]float32, 24000); for i := range s { s[i] = 1.0 }; return s }(), true},
		{"contains NaN", func() []float32 { s := sine(24000, 0.3); s[5] = float32(math.NaN()); return s }(), true},
		{"contains Inf", func() []float32 { s := sine(24000, 0.3); s[5] = float32(math.Inf(1)); return s }(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Guard(tt.samples, 24000)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Guard() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckReportsMetrics(t *testing.T) {
	r := Check(sine(24000, 0.3), 24000, Options{ExpectedSampleRate: 24000})
	if !r.OK {
		t.Fatalf("expected OK, got failures: %v", r.Failures)
	}
	if r.RMS <= 0 || r.Duration <= 0 {
		t.Fatalf("expected positive RMS/Duration, got rms=%v dur=%v", r.RMS, r.Duration)
	}
}

func TestCheckFlagsSampleRateAndDuration(t *testing.T) {
	r := Check(sine(24000, 0.3), 24000, Options{
		ExpectedSampleRate: 48000, // mismatch
		CharCount:          1000,  // 1s of audio for 1000 chars => below MinSecPerChar
		MinSecPerChar:      0.03,
		MaxSecPerChar:      0.30,
	})
	if r.OK {
		t.Fatal("expected failures for sample-rate mismatch + too-short duration")
	}
}

func TestReadWAVMono16RoundTrip(t *testing.T) {
	// Build a canonical 16-bit PCM mono WAV in memory, then decode it.
	samples := []float32{0, 0.5, -0.5, 1.0, -1.0}
	raw := writeTestWAV(samples, 24000)
	got, sr, err := ReadWAVMono16Bytes(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sr != 24000 {
		t.Fatalf("sample rate = %d, want 24000", sr)
	}
	if len(got) != len(samples) {
		t.Fatalf("len = %d, want %d", len(got), len(samples))
	}
	for i := range samples {
		if math.Abs(float64(got[i]-samples[i])) > 1.0/32767.0+1e-6 {
			t.Errorf("sample %d = %v, want ~%v", i, got[i], samples[i])
		}
	}
}
```

(`writeTestWAV` is a tiny test helper added in Step 3's test file? — no: define it inside the test file. Add it now at the bottom of `audiocheck_test.go`:)

```go
func writeTestWAV(samples []float32, sr int) []byte {
	clamp := func(s float32) int16 {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		return int16(math.Round(float64(s) * 32767))
	}
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		v := uint16(clamp(s))
		data[i*2] = byte(v)
		data[i*2+1] = byte(v >> 8)
	}
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	put32 := func(off int, v uint32) { hdr[off] = byte(v); hdr[off+1] = byte(v >> 8); hdr[off+2] = byte(v >> 16); hdr[off+3] = byte(v >> 24) }
	put16 := func(off int, v uint16) { hdr[off] = byte(v); hdr[off+1] = byte(v >> 8) }
	put32(4, uint32(36+len(data)))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	put32(16, 16)
	put16(20, 1)
	put16(22, 1)
	put32(24, uint32(sr))
	put32(28, uint32(sr*2))
	put16(32, 2)
	put16(34, 16)
	copy(hdr[36:40], "data")
	put32(40, uint32(len(data)))
	return append(hdr, data...)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd backend
go test ./internal/audiocheck/ -v
```

Expected: FAIL — `undefined: Guard`, `undefined: Check`, `undefined: Options`, `undefined: ReadWAVMono16Bytes`.

- [ ] **Step 3: Implement the package**

Create `backend/internal/audiocheck/audiocheck.go`:

```go
// Package audiocheck validates synthesized PCM audio. It backs two callers:
// the runtime Guard inside the TTS engine (turns silent-success into a loud
// job error) and the standalone Tier-0 outbox scanner. Checks are cheap and
// format-agnostic; they assume float32 samples in [-1, 1].
package audiocheck

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"
)

// Default thresholds. Tuned conservatively: real speech sits well above the
// RMS floor and well below the clip ratio; anything that trips these is
// degenerate (silence, DC, full-scale noise, NaN/Inf).
const (
	defaultRMSFloor    = 1e-4 // mean-square energy floor
	defaultMaxClipRate = 0.02 // fraction of |s| >= clipLevel tolerated
	clipLevel          = 0.999
)

// Options configures Check. Zero values disable the optional duration band.
type Options struct {
	ExpectedSampleRate int     // if > 0, mismatch is a failure
	RMSFloor           float64 // 0 => defaultRMSFloor
	MaxClipRate        float64 // 0 => defaultMaxClipRate
	CharCount          int     // input length; enables duration band when > 0
	MinSecPerChar      float64 // duration band lower bound (per char)
	MaxSecPerChar      float64 // duration band upper bound (per char)
}

// Result is the full report from Check.
type Result struct {
	OK        bool
	RMS       float64
	ClipRate  float64
	Duration  float64
	Failures  []string
}

// Check runs all applicable checks and returns a report.
func Check(samples []float32, sampleRate int, opt Options) Result {
	rmsFloor := opt.RMSFloor
	if rmsFloor == 0 {
		rmsFloor = defaultRMSFloor
	}
	maxClip := opt.MaxClipRate
	if maxClip == 0 {
		maxClip = defaultMaxClipRate
	}

	r := Result{OK: true}
	if len(samples) == 0 {
		r.OK = false
		r.Failures = append(r.Failures, "empty audio")
		return r
	}

	var sumSq float64
	var clipped int
	for _, s := range samples {
		f := float64(s)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			r.OK = false
			r.Failures = append(r.Failures, "non-finite sample (NaN/Inf)")
			return r
		}
		sumSq += f * f
		if math.Abs(f) >= clipLevel {
			clipped++
		}
	}
	r.RMS = math.Sqrt(sumSq / float64(len(samples)))
	r.ClipRate = float64(clipped) / float64(len(samples))
	if sampleRate > 0 {
		r.Duration = float64(len(samples)) / float64(sampleRate)
	}

	if r.RMS < rmsFloor {
		r.OK = false
		r.Failures = append(r.Failures, fmt.Sprintf("silent: rms %.3g < floor %.3g", r.RMS, rmsFloor))
	}
	if r.ClipRate > maxClip {
		r.OK = false
		r.Failures = append(r.Failures, fmt.Sprintf("clipped: %.1f%% of samples at full scale", r.ClipRate*100))
	}
	if opt.ExpectedSampleRate > 0 && sampleRate != opt.ExpectedSampleRate {
		r.OK = false
		r.Failures = append(r.Failures, fmt.Sprintf("sample rate %d != expected %d", sampleRate, opt.ExpectedSampleRate))
	}
	if opt.CharCount > 0 && opt.MaxSecPerChar > 0 {
		lo := float64(opt.CharCount) * opt.MinSecPerChar
		hi := float64(opt.CharCount) * opt.MaxSecPerChar
		if r.Duration < lo || r.Duration > hi {
			r.OK = false
			r.Failures = append(r.Failures, fmt.Sprintf("duration %.2fs outside band [%.2f, %.2f]s for %d chars", r.Duration, lo, hi, opt.CharCount))
		}
	}
	return r
}

// Guard is the runtime check used inside the engine: cheap, always-correct
// checks only (non-empty, finite, non-silent, not fully clipped). Sample-rate
// and duration-band checks are the standalone tool's job.
func Guard(samples []float32, sampleRate int) error {
	r := Check(samples, sampleRate, Options{})
	if !r.OK {
		return fmt.Errorf("%s", strings.Join(r.Failures, "; "))
	}
	return nil
}

// ReadWAVMono16 reads a canonical 16-bit PCM mono WAV (the layout our encoder
// and Supertonic both produce) into float32 samples in [-1, 1].
func ReadWAVMono16(path string) ([]float32, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return ReadWAVMono16Bytes(raw)
}

// ReadWAVMono16Bytes is ReadWAVMono16 for an in-memory buffer.
func ReadWAVMono16Bytes(raw []byte) ([]float32, int, error) {
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a RIFF/WAVE buffer")
	}
	if string(raw[36:40]) != "data" {
		return nil, 0, fmt.Errorf("non-canonical WAV layout (data chunk not at offset 36)")
	}
	sampleRate := int(binary.LittleEndian.Uint32(raw[24:28]))
	bits := binary.LittleEndian.Uint16(raw[34:36])
	if bits != 16 {
		return nil, 0, fmt.Errorf("expected 16-bit PCM, got %d", bits)
	}
	dataSize := int(binary.LittleEndian.Uint32(raw[40:44]))
	if 44+dataSize > len(raw) {
		dataSize = len(raw) - 44
	}
	n := dataSize / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(raw[44+i*2 : 46+i*2]))
		out[i] = float32(v) / 32768.0
	}
	return out, sampleRate, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd backend
go test ./internal/audiocheck/ -v
gofmt -l internal/audiocheck/
```

Expected: all tests `PASS`; `gofmt -l` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/audiocheck/
git commit -m "feat: add audiocheck package (silence guard + WAV reader)"
```

---

## Task 5: Rewrite `tts.Engine` to drive real inference

**Files:**
- Modify: `backend/internal/tts/supertonic.go` (full rewrite of the body; interface unchanged)
- Test: `backend/internal/tts/supertonic_test.go` (create)

The exported surface (`NewEngine`, `Voices`, `Languages`, `HasVoice`, `HasLang`, `Synthesize`, `Close`) is preserved exactly — `api/` and `audio/` need no edits. The ORT-dependent parts (`NewEngine`/`synthesizeOne`/`Close`) can only be **compile-checked** offline; `discoverVoices` and `encodeWAV` are offline-testable and get real tests.

- [ ] **Step 1: Write the failing tests (offline-testable parts only)**

Create `backend/internal/tts/supertonic_test.go`:

```go
package tts

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverVoicesFromVoiceStyles(t *testing.T) {
	dir := t.TempDir()
	vs := filepath.Join(dir, "voice_styles")
	if err := os.MkdirAll(vs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"M1.json", "F2.json", "notes.txt", "F2.json"} {
		if err := os.WriteFile(filepath.Join(vs, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := &Engine{assetsDir: dir, voiceMap: map[string]string{}}
	if err := e.discoverVoices(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"F2", "M1"}; !reflect.DeepEqual(e.Voices(), want) {
		t.Fatalf("voices = %v, want %v", e.Voices(), want)
	}
	if e.voiceMap["M1"] != filepath.Join(vs, "M1.json") {
		t.Fatalf("M1 path = %q", e.voiceMap["M1"])
	}
}

func TestDiscoverVoicesFallsBackWhenDirMissing(t *testing.T) {
	e := &Engine{assetsDir: t.TempDir(), voiceMap: map[string]string{}} // no voice_styles/
	if err := e.discoverVoices(); err != nil {
		t.Fatal(err)
	}
	if len(e.Voices()) != len(fallbackVoices()) {
		t.Fatalf("expected fallback voices, got %v", e.Voices())
	}
	if e.HasVoice("M1") == false {
		t.Fatal("fallback should include M1")
	}
}

func TestEncodeWAVRoundTripsConfiguredSampleRate(t *testing.T) {
	for _, sr := range []int{24000, 44100} {
		raw := encodeWAV([]float32{0, 0.5, -0.5, 1.5, -1.5}, sr) // last two clamp
		if got := int(binary.LittleEndian.Uint32(raw[24:28])); got != sr {
			t.Errorf("header sample rate = %d, want %d", got, sr)
		}
		dataSize := int(binary.LittleEndian.Uint32(raw[40:44]))
		if dataSize != 5*2 {
			t.Errorf("data size = %d, want 10", dataSize)
		}
		// clamp check: 1.5 -> +32767, -1.5 -> -32767
		last := int16(binary.LittleEndian.Uint16(raw[44+8 : 44+10]))
		if last != -32767 {
			t.Errorf("clamp: last sample = %d, want -32767", last)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd backend
go test ./internal/tts/ -run 'TestDiscoverVoices|TestEncodeWAV' -v
```

Expected: FAIL to **compile** — `e.discoverVoices` still references the old `voices/` layout but the struct fields are about to change; treat "build fails / wrong behaviour" as the red state. (After Step 3 these compile and pass.)

- [ ] **Step 3: Rewrite `supertonic.go`**

Replace the **entire** contents of `backend/internal/tts/supertonic.go` with:

```go
// Package tts wraps Supertonic 3 ONNX inference behind a small, stable
// interface the rest of the service depends on.
//
// The tensor orchestration lives in the vendored upstream package
// internal/tts/supertonic_native (see its VENDORED.md). This file is the
// seam: everything Supertonic-specific is reachable only through Engine, so
// the api/ and audio/ packages never import ONNX types.
//
// ONNX Runtime sessions are not goroutine-safe, so Synthesize serialises all
// inference behind e.mu. The engine is loaded once at startup and shared
// across every HTTP request and watcher event.
package tts

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/yourorg/supertonic-docx/internal/audiocheck"
	"github.com/yourorg/supertonic-docx/internal/tts/supertonic_native"
)

// Hardcoded inference parameters (design decision: not user-configurable).
const (
	totalStep       = 8             // denoising steps (upstream default)
	speed           = float32(1.05) // speech rate (upstream default)
	chunkSilenceSec = float32(0.3)  // gap between sub-chunks of long text (upstream example default)
)

// Engine is a loaded Supertonic model + voice/language catalogue. Safe for
// concurrent use; inference is serialised internally because ONNX Runtime
// sessions are not goroutine-safe.
type Engine struct {
	assetsDir string

	mu     sync.Mutex // serialises ONNX session calls and the style cache
	model  *supertonic_native.TextToSpeech
	sr     int                                  // sample rate from tts.json (AE.SampleRate)
	styles map[string]*supertonic_native.Style  // voice name -> loaded style (lazy, cached)

	voices   []string          // sorted, deduplicated
	voiceMap map[string]string // voice name -> voice_styles/<name>.json (empty when using fallback list)
	langs    []string          // from vendored AvailableLangs
}

// NewEngine loads the model assets from assetsDir.
//
// Expected layout (Supertonic 3 HF repo):
//
//	assetsDir/
//	  onnx/
//	    tts.json
//	    unicode_indexer.json
//	    text_encoder.onnx  duration_predictor.onnx
//	    vector_estimator.onnx  vocoder.onnx
//	  voice_styles/
//	    M1.json  F1.json  ...
func NewEngine(assetsDir string) (*Engine, error) {
	if _, err := os.Stat(assetsDir); err != nil {
		return nil, fmt.Errorf("assets dir %s: %w", assetsDir, err)
	}

	// Initialise ONNX Runtime once. The vendored helper resolves the library
	// from ONNXRUNTIME_LIB_PATH (fallback /usr/local/lib/libonnxruntime.so).
	if err := supertonic_native.InitializeONNXRuntime(); err != nil {
		return nil, fmt.Errorf("init ONNX Runtime (set ONNXRUNTIME_LIB_PATH if libonnxruntime is not on the default path): %w", err)
	}

	onnxDir := filepath.Join(assetsDir, "onnx")
	cfg, err := supertonic_native.LoadCfgs(onnxDir)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", filepath.Join(onnxDir, "tts.json"), err)
	}

	// LoadTextToSpeech opens the 4 ONNX sessions and loads unicode_indexer.json
	// internally; useGPU is always false (CPU-only, per design).
	model, err := supertonic_native.LoadTextToSpeech(onnxDir, false, cfg)
	if err != nil {
		return nil, fmt.Errorf("open ONNX sessions in %s: %w", onnxDir, err)
	}

	e := &Engine{
		assetsDir: assetsDir,
		model:     model,
		sr:        model.SampleRate,
		styles:    map[string]*supertonic_native.Style{},
		voiceMap:  map[string]string{},
		langs:     supportedLanguages(),
	}
	if err := e.discoverVoices(); err != nil {
		model.Destroy()
		return nil, err
	}
	return e, nil
}

// Close releases the ONNX sessions, cached style tensors, and the ORT env.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.styles {
		destroyStyle(st)
	}
	e.styles = map[string]*supertonic_native.Style{}
	if e.model != nil {
		e.model.Destroy()
		e.model = nil
	}
	ort.DestroyEnvironment()
	return nil
}

func destroyStyle(st *supertonic_native.Style) {
	if st == nil {
		return
	}
	if st.TtlTensor != nil {
		st.TtlTensor.Destroy()
	}
	if st.DpTensor != nil {
		st.DpTensor.Destroy()
	}
}

// Voices returns the available voice presets, sorted.
func (e *Engine) Voices() []string {
	out := make([]string, len(e.voices))
	copy(out, e.voices)
	return out
}

// Languages returns the supported language codes.
func (e *Engine) Languages() []string {
	out := make([]string, len(e.langs))
	copy(out, e.langs)
	return out
}

// HasVoice / HasLang validate requests before a (potentially long) job starts.
func (e *Engine) HasVoice(v string) bool { _, ok := e.voiceMap[v]; return ok }
func (e *Engine) HasLang(l string) bool {
	for _, c := range e.langs {
		if c == l {
			return true
		}
	}
	return false
}

// Synthesize generates a 16-bit PCM mono WAV for one paragraph in one
// voice/lang. Returns the raw .wav bytes ready to write to disk.
func (e *Engine) Synthesize(text, voice, lang string) ([]byte, error) {
	if !e.HasVoice(voice) {
		return nil, fmt.Errorf("unknown voice %q", voice)
	}
	if !e.HasLang(lang) {
		return nil, fmt.Errorf("unknown lang %q", lang)
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("empty text")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	samples, sr, err := e.synthesizeOne(text, voice, lang)
	if err != nil {
		return nil, err
	}
	return encodeWAV(samples, sr), nil
}

// synthesizeOne is the only function that touches Supertonic directly.
// Caller must hold e.mu.
func (e *Engine) synthesizeOne(text, voice, lang string) ([]float32, int, error) {
	style, err := e.styleFor(voice)
	if err != nil {
		return nil, 0, err
	}
	samples, _, err := e.model.Call(text, lang, style, totalStep, speed, chunkSilenceSec)
	if err != nil {
		return nil, 0, fmt.Errorf("inference: %w", err)
	}
	// Tier-0 runtime guard: turn a silent/degenerate result into a loud error
	// instead of a silently-passing job.
	if err := audiocheck.Guard(samples, e.sr); err != nil {
		return nil, 0, fmt.Errorf("degenerate audio (voice=%s lang=%s text=%q): %w", voice, lang, truncate(text, 60), err)
	}
	return samples, e.sr, nil
}

// styleFor lazily loads and caches a voice's style tensors. Caller holds e.mu.
func (e *Engine) styleFor(voice string) (*supertonic_native.Style, error) {
	if st, ok := e.styles[voice]; ok {
		return st, nil
	}
	path := e.voiceMap[voice]
	if path == "" {
		return nil, fmt.Errorf("voice %q has no style file (are assets fetched into voice_styles/?)", voice)
	}
	st, err := supertonic_native.LoadVoiceStyle([]string{path}, false)
	if err != nil {
		return nil, fmt.Errorf("load voice style %s: %w", path, err)
	}
	e.styles[voice] = st
	return st, nil
}

// discoverVoices reads assetsDir/voice_styles/*.json; voice name = filename
// without ".json". Falls back to a hardcoded preset list only when the dir is
// missing, so the UI is usable before assets are fetched.
func (e *Engine) discoverVoices() error {
	dir := filepath.Join(e.assetsDir, "voice_styles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		e.useFallbackVoices()
		return nil
	}
	seen := map[string]bool{}
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		voice := strings.TrimSuffix(ent.Name(), ".json")
		if seen[voice] {
			continue
		}
		seen[voice] = true
		e.voices = append(e.voices, voice)
		e.voiceMap[voice] = filepath.Join(dir, ent.Name())
	}
	sort.Strings(e.voices)
	if len(e.voices) == 0 {
		e.useFallbackVoices()
	}
	return nil
}

func (e *Engine) useFallbackVoices() {
	for _, v := range fallbackVoices() {
		e.voices = append(e.voices, v)
		e.voiceMap[v] = "" // no style file; styleFor will reject synthesis
	}
	sort.Strings(e.voices)
}

func fallbackVoices() []string {
	return []string{"M1", "M2", "M3", "M4", "M5", "F1", "F2", "F3", "F4", "F5"}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// encodeWAV writes a 16-bit PCM mono WAV in memory. Float samples are clamped
// to [-1, 1] then scaled to int16. The sample rate is supplied by the caller
// (from tts.json), not hardcoded.
func encodeWAV(samples []float32, sampleRate int) []byte {
	const (
		bitsPerSample = 16
		numChannels   = 1
	)
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataSize := len(samples) * 2 // int16

	buf := make([]byte, 0, 44+dataSize)
	w := func(v ...byte) { buf = append(buf, v...) }
	wU32 := func(v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		w(b[:]...)
	}
	wU16 := func(v uint16) {
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], v)
		w(b[:]...)
	}

	w([]byte("RIFF")...)
	wU32(uint32(36 + dataSize))
	w([]byte("WAVE")...)

	w([]byte("fmt ")...)
	wU32(16)
	wU16(1)
	wU16(uint16(numChannels))
	wU32(uint32(sampleRate))
	wU32(uint32(byteRate))
	wU16(uint16(blockAlign))
	wU16(uint16(bitsPerSample))

	w([]byte("data")...)
	wU32(uint32(dataSize))
	for _, s := range samples {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		i := int16(math.Round(float64(s) * 32767))
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(i))
		buf = append(buf, b[:]...)
	}
	return buf
}
```

- [ ] **Step 4: Run the offline tests + build/vet/fmt the whole module**

```bash
cd backend
go test ./internal/tts/ -run 'TestDiscoverVoices|TestEncodeWAV' -v
go build ./...
go vet ./...
gofmt -l .
```

Expected: the two named tests `PASS`; `go build`/`go vet` succeed (this is the only offline proof that the ORT-dependent code compiles); `gofmt -l` prints nothing. (Languages still come from the old `supportedLanguages()` until Task 6 — that's fine, it still compiles.)

- [ ] **Step 5: Commit**

```bash
git add backend/internal/tts/supertonic.go backend/internal/tts/supertonic_test.go
git commit -m "feat: wire tts.Engine to real Supertonic ONNX inference"
```

---

## Task 6: Drive languages off the vendored `AvailableLangs`

**Files:**
- Modify: `backend/internal/tts/languages.go`
- Modify: `backend/internal/api/server.go`
- Test: `backend/internal/tts/languages_test.go` (create)

Single source of truth = the model's own `AvailableLangs` (32 codes; note it includes `na`, which the old hand-maintained 31-code list omitted). The display-name map stays (UI-only) but the API stops duplicating it.

- [ ] **Step 1: Write the failing tests**

Create `backend/internal/tts/languages_test.go`:

```go
package tts

import (
	"testing"

	"github.com/yourorg/supertonic-docx/internal/tts/supertonic_native"
)

func TestSupportedLanguagesMatchVendored(t *testing.T) {
	got := supportedLanguages()
	want := supertonic_native.AvailableLangs
	if len(got) != len(want) {
		t.Fatalf("supportedLanguages len = %d, want %d (vendored AvailableLangs)", len(got), len(want))
	}
	set := map[string]bool{}
	for _, c := range got {
		set[c] = true
	}
	for _, c := range want {
		if !set[c] {
			t.Errorf("missing language %q from supportedLanguages()", c)
		}
	}
}

func TestEnglishStillPresent(t *testing.T) {
	e := &Engine{langs: supportedLanguages()}
	if !e.HasLang("en") {
		t.Fatal("en must be supported")
	}
}

func TestEveryLanguageResolvesToAName(t *testing.T) {
	for _, code := range supportedLanguages() {
		name := LanguageName[code]
		if name == "" {
			name = code // documented fallback (e.g. "na")
		}
		if name == "" {
			t.Errorf("language %q resolves to empty display name", code)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
cd backend
go test ./internal/tts/ -run 'TestSupportedLanguagesMatchVendored' -v
```

Expected: FAIL — the current `supportedLanguages()` returns 31 hardcoded codes, not the vendored 32.

- [ ] **Step 3: Rewrite `languages.go`**

Replace the entire contents of `backend/internal/tts/languages.go` with:

```go
package tts

import "github.com/yourorg/supertonic-docx/internal/tts/supertonic_native"

// supportedLanguages returns the language codes the model accepts, taken
// directly from the vendored engine so there is a single source of truth.
func supportedLanguages() []string {
	out := make([]string, len(supertonic_native.AvailableLangs))
	copy(out, supertonic_native.AvailableLangs)
	return out
}

// LanguageName maps ISO codes to English display names for the UI. Codes with
// no entry here (e.g. the model's "na") fall back to the code itself at the
// API layer; the model doesn't care what we label them.
var LanguageName = map[string]string{
	"ar": "Arabic", "bg": "Bulgarian", "cs": "Czech", "da": "Danish",
	"de": "German", "el": "Greek", "en": "English", "es": "Spanish",
	"et": "Estonian", "fi": "Finnish", "fr": "French", "hi": "Hindi",
	"hr": "Croatian", "hu": "Hungarian", "id": "Indonesian", "it": "Italian",
	"ja": "Japanese", "ko": "Korean", "lt": "Lithuanian", "lv": "Latvian",
	"nl": "Dutch", "pl": "Polish", "pt": "Portuguese", "ro": "Romanian",
	"ru": "Russian", "sk": "Slovak", "sl": "Slovenian", "sv": "Swedish",
	"tr": "Turkish", "uk": "Ukrainian", "vi": "Vietnamese",
}
```

- [ ] **Step 4: Dedupe the language-name table in `server.go`**

In `backend/internal/api/server.go`, add the `tts` import (it is already an indirect dependency of the package via `jobs.go`; no import cycle — `tts` does not import `api`):

```go
import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/yourorg/supertonic-docx/internal/tts"
)
```

Replace the whole `catalogueLangs` function (and delete its inline `names` map + the stale import-cycle comment) with:

```go
func catalogueLangs(codes []string) []langEntry {
	out := make([]langEntry, 0, len(codes))
	for _, c := range codes {
		name, ok := tts.LanguageName[c]
		if !ok || name == "" {
			name = c
		}
		out = append(out, langEntry{Code: c, Name: name})
	}
	return out
}
```

- [ ] **Step 5: Run tests + build/vet/fmt**

```bash
cd backend
go test ./internal/tts/ -v
go build ./...
go vet ./...
gofmt -l .
```

Expected: all `tts` tests `PASS`; build/vet clean; `gofmt -l` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/tts/languages.go backend/internal/tts/languages_test.go backend/internal/api/server.go
git commit -m "refactor: drive languages off vendored AvailableLangs (single source of truth)"
```

---

## Task 7: Bundle the ONNX Runtime native library in Docker

**Files:**
- Modify: `Dockerfile`
- Create: `.dockerignore`
- Modify: `docker-compose.yml`

Target arch is **linux/arm64** only (per design). The Go binary stays `CGO_ENABLED=0` (the binding dlopens the `.so`); a documented contingency switches to cgo if that assumption is wrong.

- [ ] **Step 1: Create `.dockerignore`**

Without this, the ~200 MB `assets/` model tree and the `outbox/` data ship into every build context. Create `.dockerignore` at repo root:

```
.git
.gitignore
.idea
.DS_Store
**/.DS_Store
assets
inbox
outbox
examples
docs
*.log
backend/server
```

(Keep `frontend/` and `backend/` — the Dockerfile COPYs them.)

- [ ] **Step 2: Rewrite the `Dockerfile`**

Replace the entire `Dockerfile` with:

```dockerfile
# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.22-bookworm AS build
ARG ONNXRUNTIME_VERSION=1.16.0
WORKDIR /src

# Fetch the ONNX Runtime native library (aarch64) to bundle into the runtime
# image. The binding (yalue/onnxruntime_go) dlopens this at runtime.
RUN set -eux; \
    curl -fsSL -o /tmp/ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-aarch64-${ONNXRUNTIME_VERSION}.tgz"; \
    mkdir -p /opt/onnxruntime; \
    tar -xzf /tmp/ort.tgz -C /opt/onnxruntime --strip-components=1; \
    rm /tmp/ort.tgz

# Dependency layer first for cache hits.
COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ ./
# CGO_ENABLED=0: the ONNX binding loads libonnxruntime via purego/dlopen, so no
# C toolchain is needed. CONTINGENCY: if this ever fails with an "import "C"" or
# linker error (a future binding version reintroducing cgo), set
# CGO_ENABLED=1 and add `gcc` here:
#   RUN apt-get update && apt-get install -y --no-install-recommends gcc && rm -rf /var/lib/apt/lists/*
#   RUN CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /out/server ./cmd/server

# ---- runtime stage ----
FROM debian:bookworm-slim

# ca-certificates for HTTPS; ffmpeg is for users' optional post-processing
# (the pipeline itself does not call it).
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# Bundle the ONNX Runtime shared library and register it with the linker.
COPY --from=build /opt/onnxruntime/lib/libonnxruntime.so* /usr/local/lib/
RUN ldconfig

WORKDIR /app
COPY --from=build /out/server /app/server
COPY frontend /app/frontend

# Placeholder dirs for the bind mounts to land on.
RUN mkdir -p /app/assets /app/inbox /app/outbox

ENV SUPERTONIC_ASSETS=/app/assets \
    SUPERTONIC_INBOX=/app/inbox \
    SUPERTONIC_OUTBOX=/app/outbox \
    SUPERTONIC_FRONTEND=/app/frontend \
    SUPERTONIC_PORT=8080 \
    ONNXRUNTIME_LIB_PATH=/usr/local/lib/libonnxruntime.so

EXPOSE 8080
ENTRYPOINT ["/app/server"]
```

> **Known deviation (not a TODO):** the container still runs as root. A non-root `USER` is good practice, but the bind-mounted `outbox/`/`assets/` are owned by the host user, so a non-root container user can hit permission errors writing output on this single-user local tool. Left as root deliberately; revisit if this is ever deployed multi-user.

- [ ] **Step 3: Pin the platform in `docker-compose.yml`**

In `docker-compose.yml`, under the `supertonic-docx` service, add a `platform` key (so the aarch64 `.so` and `GOARCH=arm64` binary match the image):

```yaml
  supertonic-docx:
    build:
      context: .
      dockerfile: Dockerfile
    image: supertonic-docx:latest
    container_name: supertonic-docx
    platform: linux/arm64
    restart: unless-stopped
    ports:
      - "8787:8080"
```

(Leave the rest of the file as-is. No `ONNXRUNTIME_LIB_PATH` needed in compose — the Dockerfile sets it.)

- [ ] **Step 4: Build the image (USER-RUN — needs network + arm64 host)**

```bash
docker compose build
```

Expected: build succeeds; the ONNX Runtime tarball downloads and `libonnxruntime.so*` lands in `/usr/local/lib`. **If the Go build fails on a cgo/`import "C"` error, apply the CONTINGENCY block in the Dockerfile comment** (CGO_ENABLED=1 + gcc) and report it back — that means the purego assumption was wrong for this binding version.

- [ ] **Step 5: Commit**

```bash
git add Dockerfile .dockerignore docker-compose.yml
git commit -m "build: bundle ONNX Runtime v1.16.0 aarch64 in the image"
```

---

## Task 8: Tier 0 standalone outbox scanner (USER-RUN)

**Files:**
- Create: `backend/eval/audiocheck/main.go`

Scans an outbox job directory and reports per-WAV pass/fail using the shared `audiocheck` package + `source.json` for the duration band. Compiles offline; the real scan is user-run after generating audio.

- [ ] **Step 1: Write the tool**

Create `backend/eval/audiocheck/main.go`:

```go
// Command eval-audiocheck scans an outbox job dir (or any dir tree of WAVs)
// and reports Tier-0 audio sanity per file: non-silent, finite, not clipped,
// sample rate == expected, duration within a per-char band.
//
// Usage:
//   go run ./eval/audiocheck -dir ../outbox/job-<id> -sr 24000
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yourorg/supertonic-docx/internal/audiocheck"
)

type sourceManifest struct {
	Paragraphs []struct {
		Index int    `json:"index"`
		Text  string `json:"text"`
	} `json:"paragraphs"`
}

func main() {
	dir := flag.String("dir", "", "job dir to scan (e.g. ../outbox/job-<id>)")
	sr := flag.Int("sr", 0, "expected sample rate (0 = skip the check)")
	minSPC := flag.Float64("min-sec-per-char", 0.015, "duration band lower bound per char")
	maxSPC := flag.Float64("max-sec-per-char", 0.40, "duration band upper bound per char")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "missing -dir")
		os.Exit(2)
	}

	// Map para index -> char count from source.json, if present.
	chars := map[int]int{}
	if b, err := os.ReadFile(filepath.Join(*dir, "source.json")); err == nil {
		var sm sourceManifest
		if json.Unmarshal(b, &sm) == nil {
			for _, p := range sm.Paragraphs {
				chars[p.Index] = len([]rune(p.Text))
			}
		}
	}

	var total, failed int
	err := filepath.WalkDir(*dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".wav") {
			return nil
		}
		total++
		samples, gotSR, err := audiocheck.ReadWAVMono16(path)
		if err != nil {
			failed++
			fmt.Printf("FAIL  %s: %v\n", path, err)
			return nil
		}
		opt := audiocheck.Options{ExpectedSampleRate: *sr}
		// Duration band only for per-paragraph files we have char counts for.
		if base := filepath.Base(path); strings.HasPrefix(base, "para_") {
			var idx int
			fmt.Sscanf(base, "para_%03d.wav", &idx)
			if c, ok := chars[idx]; ok && c > 0 {
				opt.CharCount = c
				opt.MinSecPerChar = *minSPC
				opt.MaxSecPerChar = *maxSPC
			}
		}
		r := audiocheck.Check(samples, gotSR, opt)
		if r.OK {
			fmt.Printf("ok    %s  rms=%.3g dur=%.2fs sr=%d\n", path, r.RMS, r.Duration, gotSR)
		} else {
			failed++
			fmt.Printf("FAIL  %s: %s\n", path, strings.Join(r.Failures, "; "))
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("\n%d files, %d failed\n", total, failed)
	if failed > 0 {
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Compile-check + vet + fmt (offline obligation)**

```bash
cd backend
go build ./eval/audiocheck/
go vet ./eval/audiocheck/
gofmt -l eval/audiocheck/
```

Expected: clean.

- [ ] **Step 3: Run against a generated job (USER-RUN)**

```bash
# after generating audio with real assets:
cd backend
go run ./eval/audiocheck -dir ../outbox/job-<id> -sr <AE.SampleRate from tts.json>
```

Expected (when inference works): every WAV `ok`, exit 0. With the old stub it would report `silent` for all — that is the Tier-0 contract working.

- [ ] **Step 4: Commit**

```bash
git add backend/eval/audiocheck/
git commit -m "feat: add Tier 0 standalone audio sanity scanner"
```

---

## Task 9: Tier 1 seed-pinned wrapper parity test (USER-RUN, build-tagged)

**Files:**
- Create: `backend/internal/tts/parity_model_test.go`

Validates the **Engine wrapper** (asset loading, style selection, lang passing, hardcoded params) against a direct vendored call with the same pinned seed. Guarded by `//go:build model_evals` so it never runs in the default `go test ./...`.

- [ ] **Step 1: Write the parity test**

Create `backend/internal/tts/parity_model_test.go`:

```go
//go:build model_evals

// Tier 1 parity. Run only with real assets + ONNX Runtime present:
//   SUPERTONIC_ASSETS=../../../assets go test -tags model_evals ./internal/tts -run Parity -v
package tts

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/yourorg/supertonic-docx/internal/tts/supertonic_native"
)

const parityText = "This morning, I took a walk in the park." // upstream example default

func TestParityWrapperVsDirect(t *testing.T) {
	assets := os.Getenv("SUPERTONIC_ASSETS")
	if assets == "" {
		t.Skip("set SUPERTONIC_ASSETS to the model dir to run parity")
	}
	const (
		voice = "M1"
		lang  = "en"
		seed  = int64(1234)
	)

	// Engine side: NewEngine initialises ORT globally and opens sessions.
	e, err := NewEngine(assets)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer e.Close()
	e.model.SeedFunc = func() int64 { return seed } // pin (white-box)

	got, sr, err := e.synthesizeOne(parityText, voice, lang)
	if err != nil {
		t.Fatalf("synthesizeOne: %v", err)
	}

	// Direct side: ORT env is already initialised by NewEngine, so just load a
	// second model + style and Call with the same params + seed.
	onnxDir := filepath.Join(assets, "onnx")
	cfg, err := supertonic_native.LoadCfgs(onnxDir)
	if err != nil {
		t.Fatalf("LoadCfgs: %v", err)
	}
	model, err := supertonic_native.LoadTextToSpeech(onnxDir, false, cfg)
	if err != nil {
		t.Fatalf("LoadTextToSpeech: %v", err)
	}
	defer model.Destroy()
	model.SeedFunc = func() int64 { return seed }

	style, err := supertonic_native.LoadVoiceStyle([]string{filepath.Join(assets, "voice_styles", voice+".json")}, false)
	if err != nil {
		t.Fatalf("LoadVoiceStyle: %v", err)
	}
	defer func() {
		style.TtlTensor.Destroy()
		style.DpTensor.Destroy()
	}()

	want, _, err := model.Call(parityText, lang, style, totalStep, speed, chunkSilenceSec)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if sr != model.SampleRate {
		t.Fatalf("sample rate mismatch: engine %d vs direct %d", sr, model.SampleRate)
	}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: engine %d vs direct %d", len(got), len(want))
	}
	var sumSq float64
	for i := range got {
		d := float64(got[i] - want[i])
		sumSq += d * d
	}
	rmsDiff := math.Sqrt(sumSq / float64(len(got)))
	if rmsDiff > 1e-3 {
		t.Fatalf("RMS diff %.3g exceeds 1e-3 — wrapper diverges from direct call", rmsDiff)
	}
	t.Logf("parity OK: %d samples, rms diff %.3g", len(got), rmsDiff)
}
```

- [ ] **Step 2: Compile-check with the build tag (offline obligation)**

```bash
cd backend
go vet -tags model_evals ./internal/tts/                 # type-checks the tagged test file
go test -tags model_evals ./internal/tts/ -run '^$'      # compiles tests, matches none -> runs nothing
```

Expected: both succeed (the second prints `ok ... [no tests to run]`). This proves the tagged test compiles without executing inference.

- [ ] **Step 3: Run with real assets (USER-RUN)**

```bash
cd backend
SUPERTONIC_ASSETS=../assets go test -tags model_evals ./internal/tts -run Parity -v
```

Expected: `PASS` with `parity OK: <N> samples, rms diff <small>`.

- [ ] **Step 4: Commit**

```bash
git add backend/internal/tts/parity_model_test.go
git commit -m "test: add Tier 1 seed-pinned wrapper parity (model_evals tag)"
```

---

## Task 10: Tier 2 intelligibility round-trip (WER/CER, USER-RUN)

**Files:**
- Create: `backend/eval/wer/metrics.go`
- Test: `backend/eval/wer/metrics_test.go`
- Create: `backend/eval/wer/main.go`
- Create: `backend/eval/wer/corpus.json`
- Create: `backend/eval/wer/thresholds.json`
- Create: `backend/eval/asr.sh.example`
- Create: `backend/eval/README.md`

The metrics are pure functions (offline TDD). The driver synthesizes via the real `Engine`, shells out to a user-supplied ASR wrapper, and scores. ASR is pluggable so we don't hardcode a brittle Whisper invocation.

- [ ] **Step 1: Write the failing metrics tests**

Create `backend/eval/wer/metrics_test.go`:

```go
package main

import (
	"math"
	"testing"
)

func TestNormalize(t *testing.T) {
	got := normalize("Hello, World!  It's  $5.")
	want := "hello world its 5"
	if got != want {
		t.Fatalf("normalize = %q, want %q", got, want)
	}
}

func TestWER(t *testing.T) {
	tests := []struct {
		ref, hyp string
		want     float64
	}{
		{"the cat sat", "the cat sat", 0.0},
		{"the cat sat", "the dog sat", 1.0 / 3.0},        // 1 sub / 3 ref words
		{"the cat sat on the mat", "the cat on mat", 2.0 / 6.0}, // 2 deletions
		{"", "", 0.0},
	}
	for _, tt := range tests {
		if got := WER(tt.ref, tt.hyp); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("WER(%q,%q) = %v, want %v", tt.ref, tt.hyp, got, tt.want)
		}
	}
}

func TestCER(t *testing.T) {
	if got := CER("abc", "abc"); got != 0 {
		t.Errorf("CER identical = %v, want 0", got)
	}
	if got := CER("abc", "abd"); math.Abs(got-1.0/3.0) > 1e-9 {
		t.Errorf("CER 1 sub = %v, want 1/3", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
cd backend
go test ./eval/wer/ -v
```

Expected: FAIL — `undefined: normalize`, `WER`, `CER`.

- [ ] **Step 3: Implement the metrics**

Create `backend/eval/wer/metrics.go`:

```go
package main

import (
	"regexp"
	"strings"
)

var nonAlnum = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)
var spaces = regexp.MustCompile(`\s+`)

// normalize lowercases, strips punctuation/symbols, and collapses whitespace.
// Unicode-aware so it works for de/ja as well as en.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = nonAlnum.ReplaceAllString(s, "")
	s = spaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func levenshtein(a, b []string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// WER = word edit distance / reference word count.
func WER(ref, hyp string) float64 {
	r := strings.Fields(normalize(ref))
	h := strings.Fields(normalize(hyp))
	if len(r) == 0 {
		if len(h) == 0 {
			return 0
		}
		return 1
	}
	return float64(levenshtein(r, h)) / float64(len(r))
}

// CER = character edit distance / reference char count (spaces removed).
func CER(ref, hyp string) float64 {
	r := strings.Split(strings.ReplaceAll(normalize(ref), " ", ""), "")
	h := strings.Split(strings.ReplaceAll(normalize(hyp), " ", ""), "")
	if len(r) == 0 {
		if len(h) == 0 {
			return 0
		}
		return 1
	}
	return float64(levenshtein(r, h)) / float64(len(r))
}
```

- [ ] **Step 4: Run the metrics tests to verify they pass**

```bash
cd backend
go test ./eval/wer/ -v
```

Expected: all `PASS`.

- [ ] **Step 5: Write the corpus and thresholds**

Create `backend/eval/wer/corpus.json` (en + de + ja, ≥2 voices each, plus edge texts — numbers/currency/abbreviations/long paragraph):

```json
[
  { "id": "en-m1", "lang": "en", "voice": "M1", "text": "The quick brown fox jumps over the lazy dog near the riverbank." },
  { "id": "en-f1", "lang": "en", "voice": "F1", "text": "She sells seashells by the seashore on a bright summer morning." },
  { "id": "en-edge-num", "lang": "en", "voice": "M1", "text": "The invoice total was 1,250 dollars and 99 cents, due by March 3rd, 2026." },
  { "id": "en-edge-abbr", "lang": "en", "voice": "F1", "text": "Dr. Smith, e.g., reviewed the Q3 report, i.e., the third-quarter results." },
  { "id": "en-long", "lang": "en", "voice": "M1", "text": "Across the wide valley the morning mist slowly lifted, revealing fields of wheat that swayed gently in the breeze, while in the distance a single church bell rang out, marking the start of another quiet day in the countryside." },
  { "id": "de-m1", "lang": "de", "voice": "M1", "text": "Der schnelle braune Fuchs springt über den faulen Hund am Flussufer." },
  { "id": "de-f1", "lang": "de", "voice": "F1", "text": "Am frühen Morgen ging sie über die Brücke und kaufte frisches Brot." },
  { "id": "ja-m1", "lang": "ja", "voice": "M1", "text": "今日はとても良い天気なので、公園を散歩しました。" },
  { "id": "ja-f1", "lang": "ja", "voice": "F1", "text": "新しい本を買って、家でゆっくり読みました。" }
]
```

Create `backend/eval/wer/thresholds.json`:

```json
{
  "en": { "metric": "wer", "max": 0.15 },
  "de": { "metric": "wer", "max": 0.20 },
  "ja": { "metric": "cer", "max": 0.15 }
}
```

> If a corpus voice isn't present in your assets, change its `voice` to one that is (run `eval-audiocheck` listing or check `assets/voice_styles/`).

- [ ] **Step 6: Write the driver**

Create `backend/eval/wer/main.go`:

```go
// Command eval-wer is the Tier-2 intelligibility round-trip: synthesize each
// corpus entry with the real Engine, transcribe with a user-supplied ASR
// wrapper, and score WER/CER against per-language thresholds.
//
// Usage:
//   SUPERTONIC_ASSETS=../assets go run ./eval/wer \
//     -corpus eval/wer/corpus.json -thresholds eval/wer/thresholds.json \
//     -asr eval/asr.sh -out /tmp/wer-out
//
// The ASR wrapper is exec'd as:  <asr> <wav-path> <lang>  and must print the
// transcript to stdout. See eval/asr.sh.example.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yourorg/supertonic-docx/internal/tts"
)

type corpusEntry struct {
	ID    string `json:"id"`
	Lang  string `json:"lang"`
	Voice string `json:"voice"`
	Text  string `json:"text"`
}

type threshold struct {
	Metric string  `json:"metric"` // "wer" | "cer"
	Max    float64 `json:"max"`
}

func main() {
	assets := flag.String("assets", os.Getenv("SUPERTONIC_ASSETS"), "model assets dir")
	corpusPath := flag.String("corpus", "eval/wer/corpus.json", "corpus JSON")
	thrPath := flag.String("thresholds", "eval/wer/thresholds.json", "thresholds JSON")
	asr := flag.String("asr", "eval/asr.sh", "ASR wrapper: <asr> <wav> <lang> -> transcript on stdout")
	outDir := flag.String("out", "/tmp/wer-out", "where to write generated WAVs")
	flag.Parse()
	if *assets == "" {
		fmt.Fprintln(os.Stderr, "missing -assets / SUPERTONIC_ASSETS")
		os.Exit(2)
	}

	corpus := mustLoadCorpus(*corpusPath)
	thresholds := mustLoadThresholds(*thrPath)

	eng, err := tts.NewEngine(*assets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewEngine: %v\n", err)
		os.Exit(1)
	}
	defer eng.Close()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var failures int
	for _, e := range corpus {
		wav, err := eng.Synthesize(e.Text, e.Voice, e.Lang)
		if err != nil {
			failures++
			fmt.Printf("FAIL  %s: synth: %v\n", e.ID, err)
			continue
		}
		wavPath := filepath.Join(*outDir, e.ID+".wav")
		if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		transcript, err := transcribe(*asr, wavPath, e.Lang)
		if err != nil {
			failures++
			fmt.Printf("FAIL  %s: asr: %v\n", e.ID, err)
			continue
		}

		thr, ok := thresholds[e.Lang]
		if !ok {
			fmt.Printf("WARN  %s: no threshold for lang %q; skipping gate\n", e.ID, e.Lang)
			continue
		}
		var score float64
		switch thr.Metric {
		case "cer":
			score = CER(e.Text, transcript)
		default:
			score = WER(e.Text, transcript)
		}
		status := "ok  "
		if score > thr.Max {
			status = "FAIL"
			failures++
		}
		fmt.Printf("%s  %s  %s=%.3f (max %.3f)  hyp=%q\n", status, e.ID, thr.Metric, score, thr.Max, transcript)
	}

	fmt.Printf("\n%d entries, %d failed\n", len(corpus), failures)
	if failures > 0 {
		os.Exit(1)
	}
}

func transcribe(asr, wav, lang string) (string, error) {
	out, err := exec.Command(asr, wav, lang).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func mustLoadCorpus(path string) []corpusEntry {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var c []corpusEntry
	if err := json.Unmarshal(b, &c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return c
}

func mustLoadThresholds(path string) map[string]threshold {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var t map[string]threshold
	if err := json.Unmarshal(b, &t); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return t
}
```

- [ ] **Step 7: Write the ASR wrapper example**

Create `backend/eval/asr.sh.example`:

```bash
#!/usr/bin/env bash
# asr.sh — transcribe $1 (a WAV) in language $2, print transcript to stdout.
# Copy to asr.sh and adapt to YOUR Whisper install. Example: whisper.cpp.
set -euo pipefail
WAV="$1"
LANG="$2"
MODEL="${WHISPER_MODEL:-$HOME/whisper.cpp/models/ggml-large-v3.bin}"

# whisper.cpp: -nt = no timestamps, prints plain text to stdout.
whisper-cli -m "$MODEL" -l "$LANG" -nt -f "$WAV" 2>/dev/null

# --- OpenAI whisper (Python) alternative ---
# whisper "$WAV" --language "$LANG" --model small --output_format txt --output_dir /tmp/whisper-out >/dev/null 2>&1
# cat "/tmp/whisper-out/$(basename "${WAV%.wav}").txt"
```

- [ ] **Step 8: Write `eval/README.md`**

Create `backend/eval/README.md`:

```markdown
# Evals

Run after fetching real assets (`../scripts/fetch-model.sh`). All three tiers
need the model + ONNX Runtime, so they run on your machine, not in CI.

`SR` below = `AE.SampleRate` in `assets/onnx/tts.json`.

## Tier 0 — audio sanity (after generating any job)
    go run ./eval/audiocheck -dir ../outbox/job-<id> -sr <SR>
Also enforced at runtime: a silent/degenerate paragraph fails the job loudly.

## Tier 1 — wrapper parity (seed-pinned)
    SUPERTONIC_ASSETS=../assets go test -tags model_evals ./internal/tts -run Parity -v

## Tier 2 — intelligibility (WER/CER)
    cp eval/asr.sh.example eval/asr.sh && chmod +x eval/asr.sh   # adapt to your Whisper
    SUPERTONIC_ASSETS=../assets go run ./eval/wer \
      -corpus eval/wer/corpus.json -thresholds eval/wer/thresholds.json \
      -asr eval/asr.sh -out /tmp/wer-out

## Acceptance gate
"Done — verified" = Tier 0 green on a real run AND Tier 2 clears the `en` WER
threshold (≤ 0.15). Tier 1 is a wrapper-debugging aid; manual listening is a
backstop, not the gate.
```

- [ ] **Step 9: Compile-check everything + run metrics tests (offline obligation)**

```bash
cd backend
go build ./eval/...
go vet ./eval/...
go test ./eval/wer/ -v
gofmt -l eval/
chmod +x eval/asr.sh.example
```

Expected: build/vet clean, metrics tests `PASS`, `gofmt -l` prints nothing.

- [ ] **Step 10: Run Tier 2 with real assets + Whisper (USER-RUN)**

```bash
cd backend
cp eval/asr.sh.example eval/asr.sh && chmod +x eval/asr.sh   # adapt first
SUPERTONIC_ASSETS=../assets go run ./eval/wer -asr eval/asr.sh
```

Expected: per-entry scores; exit 0 when every language clears its threshold.

- [ ] **Step 11: Commit**

```bash
git add backend/eval/
git commit -m "feat: add Tier 2 WER/CER intelligibility eval"
```

---

## Task 11: Documentation + fetch-model sanity check

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`
- Modify: `scripts/fetch-model.sh`

Remove every "stubbed / returns silence" claim and document the real native dependency, asset layout, and evals.

- [ ] **Step 1: README — remove the stub warning block**

In `README.md`, delete the blockquote warning (the `> ⚠️ **Read this before you expect audio:** ...` paragraph, lines ~12–15) entirely.

- [ ] **Step 2: README — fix the "What working looks like" paragraph**

Replace the paragraph that begins `**What "working" looks like right now:**` (and its claim that WAVs "play as **silence**") with:

```markdown
**What "working" looks like:** upload a `.docx`, watch the progress bar tick to 100%,
click through to per-paragraph WAVs and a stitched `full.wav` — and hear real speech.
If audio comes back silent, the engine now fails the job loudly (Tier-0 guard) rather
than producing silent files; check that `./assets` was populated by `fetch-model.sh`.
```

- [ ] **Step 3: README — replace the "Current status" section**

Replace the entire `## Current status` section with:

```markdown
## Status

Inference is live: a vendored copy of Supertone's upstream Go engine
(`backend/internal/tts/supertonic_native/`, MIT — see its `VENDORED.md`) runs the
4-model Supertonic 3 ONNX pipeline (`text_encoder → duration_predictor →
vector_estimator → vocoder`) in-process via
[`onnxruntime_go`](https://github.com/yalue/onnxruntime_go).

**Native dependency:** the ONNX Runtime C library. The Docker image bundles
v1.16.0 (aarch64) at `/usr/local/lib/libonnxruntime.so` and sets
`ONNXRUNTIME_LIB_PATH`. For a bare local run, install ONNX Runtime and point
`ONNXRUNTIME_LIB_PATH` at the library (macOS: `brew install onnxruntime`).

**Verification:** see `backend/eval/README.md` for the three eval tiers
(audio sanity, seed-pinned parity, Whisper WER/CER).
```

- [ ] **Step 4: README — fix asset layout, voices, languages, sample rate**

- In the **Repo layout** block, change `internal/tts/   Supertonic Engine (the stubbed ONNX seam) + language catalogue` to `internal/tts/   Supertonic Engine (real ONNX inference) + vendored engine + language catalogue` and change `assets/   Supertonic 3 ONNX + voice presets (gitignored — fetched)` to `assets/   onnx/ (4 models + tts.json + unicode_indexer.json) + voice_styles/*.json (gitignored — fetched)`.
- In the **Voices & languages** section, change the Voices bullet to: "discovered at runtime from `assets/voice_styles/*.json` (voice name = filename without `.json`); drop new ones in and they appear." Change the Languages bullet to: "supplied by the model (`AvailableLangs` in the vendored engine); the API exposes them via `/api/catalogue`."
- Anywhere the README states 44.1 kHz as fixed, note the sample rate comes from `assets/onnx/tts.json` (`AE.SampleRate`).
- In the **Configuration** table, change the `SUPERTONIC_ASSETS` row meaning to `Path to assets (onnx/ + voice_styles/)` and add a row: `| ONNXRUNTIME_LIB_PATH | /usr/local/lib/libonnxruntime.so | Path to the ONNX Runtime C library (set in the image) |`.

- [ ] **Step 5: CLAUDE.md — replace "The one thing to know first"**

Replace the entire `## The one thing to know first` section with:

```markdown
## The one thing to know first

**TTS inference is real.** `backend/internal/tts/supertonic.go` drives a vendored copy of
Supertone's upstream Go engine (`internal/tts/supertonic_native/`, MIT) running the 4-model
Supertonic 3 ONNX pipeline via `yalue/onnxruntime_go`. That binding dlopens the ONNX Runtime
C library at runtime (`ONNXRUNTIME_LIB_PATH`, default `/usr/local/lib/libonnxruntime.so`;
bundled v1.16.0 aarch64 in the image). Assets live under `assets/onnx/` (4 `.onnx` + `tts.json`
+ `unicode_indexer.json`) and `assets/voice_styles/*.json`. The `tts.Engine` interface is the
seam: `api/` and `audio/` never see ONNX types. Evals (audio sanity, seed parity, Whisper WER)
are in `backend/eval/` — see `backend/eval/README.md`.
```

- [ ] **Step 6: CLAUDE.md — fix the gotchas that reference the stub/old layout**

- In **Voices vs. languages**, change "discovered at runtime from `assets/voices/*.{bin,npy,onnx}`" to "discovered at runtime from `assets/voice_styles/*.json`", and replace the languages paragraph (the hand-maintained 31-code list + duplicated map) with: "Languages come from the vendored engine's `AvailableLangs` (`languages.go` wraps it); display names live in `tts.LanguageName`, reused by `api/server.go` — no duplication." 
- In **WAV format**, change "16-bit PCM, mono, 44.1 kHz" to "16-bit PCM, mono; sample rate from `tts.json` (`AE.SampleRate`), threaded through `encodeWAV`".
- Update the **Commands** block: change the `go test ./...` comment from "no tests exist yet" to "offline unit tests (tts, audiocheck, eval/wer); model evals are tagged `model_evals` and run separately — see backend/eval/README.md".

- [ ] **Step 7: Add a post-clone sanity check to `fetch-model.sh`**

In `scripts/fetch-model.sh`, replace the final `echo "Contents:"` block with a check that the real layout landed:

```bash
echo
echo "Done. Assets live at: $DEST"
for p in onnx/tts.json onnx/unicode_indexer.json onnx/text_encoder.onnx voice_styles; do
  if [ -e "$DEST/$p" ]; then
    echo "  ✓ $p"
  else
    echo "  ✗ MISSING: $p  (asset layout may differ from expectations — check the HF repo tree)"
  fi
done
```

- [ ] **Step 8: Verify docs build nothing but stay consistent**

```bash
cd backend
go build ./...        # sanity: nothing in docs breaks the build
grep -rn "stub\|silence\|44100\|voices/\*" README.md CLAUDE.md || echo "no stale references"
```

Expected: build clean; the grep finds no remaining stub/silence/44100/`voices/*` references (a `voice_styles` mention is fine).

- [ ] **Step 9: Commit**

```bash
git add README.md CLAUDE.md scripts/fetch-model.sh
git commit -m "docs: document real inference, native dep, asset layout, and evals"
```

---

## Task 12: Full offline verification sweep

**Files:** none (verification only)

- [ ] **Step 1: Build, vet, format, test the whole module**

```bash
cd backend
go build ./...
go vet ./...
gofmt -l .
go test ./...
```

Expected: build/vet succeed; `gofmt -l` prints nothing; `go test ./...` passes (the offline tests in `internal/tts`, `internal/audiocheck`, `internal/tts/supertonic_native`, `eval/wer`). The `model_evals`-tagged parity test is excluded from the default run by design.

- [ ] **Step 2: Compile-check the tagged + eval code paths**

```bash
cd backend
go vet -tags model_evals ./...
go build ./eval/...
```

Expected: clean.

- [ ] **Step 3: Confirm the public interface is unchanged**

```bash
cd backend
go doc ./internal/tts Engine
```

Expected: `NewEngine`, `Voices`, `Languages`, `HasVoice`, `HasLang`, `Synthesize`, `Close` present with the same signatures — confirming `api/` and `audio/` needed no edits.

- [ ] **Step 4: Hand off the model-dependent run sequence to the user**

Report that offline verification is green and the following must be run by the user (with assets + ONNX Runtime), per the design's acceptance gate:

```bash
./scripts/fetch-model.sh
docker compose up --build           # → http://localhost:8787, drop a .docx
# then, from backend/, with SUPERTONIC_ASSETS pointed at ./assets:
go run ./eval/audiocheck -dir ../outbox/job-<id> -sr <SR>      # Tier 0
go test -tags model_evals ./internal/tts -run Parity -v        # Tier 1
go run ./eval/wer -asr eval/asr.sh                             # Tier 2
```

**Acceptance gate:** Tier 0 green on a real run AND Tier 2 clears the `en` WER threshold (≤ 0.15). Report results/errors verbatim; iterate on environment issues (asset layout, `.so` arch, CGO contingency) from there.

---

## Self-review notes (spec coverage)

- **Component A (vendor + seed hook + LICENSE + VENDORED.md + integrity):** Tasks 1–3.
- **Component B (Engine rewrite, interface preserved, sample rate from config):** Task 5. (Refinement vs spec: `unicode_indexer.json` is loaded inside `LoadTextToSpeech`, so `NewEngine` only calls `LoadCfgs` + `LoadTextToSpeech` — no separate indexer load. `Call` takes the 6th `silenceDuration` arg, hardcoded `0.3` per upstream.)
- **Component C (Docker native lib, arm64, CGO_ENABLED=0 + contingency):** Task 7 (+ `.dockerignore`).
- **Component D (voice_styles discovery; AvailableLangs single source of truth):** Tasks 5–6.
- **Component E (docs):** Task 11.
- **Eval Tier 0 (runtime guard + standalone tool):** Task 4 (`audiocheck`), Task 5 (guard wired in `synthesizeOne`), Task 8 (standalone scanner).
- **Eval Tier 1 (seed-pinned parity):** Task 9.
- **Eval Tier 2 (Whisper WER/CER, en+de+ja × ≥2 voices + edge texts):** Task 10.
- **Plumbing tests (build/vet/gofmt; voice discovery; languages; encodeWAV round-trip; vendoring integrity):** Tasks 3, 4, 5, 6, 12.
- **Acceptance gate + user run sequence:** Task 12.
```
