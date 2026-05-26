# Design: Wire in real Supertonic 3 ONNX inference

- **Date:** 2026-05-26
- **Status:** Approved (pending spec review)
- **Topic:** Replace the stubbed `synthesizeOne` in `backend/internal/tts/supertonic.go`
  with real Supertonic 3 inference, keeping the single-Go-binary design.

## Goal & success criteria

A user opens the web UI, drops a `.docx`, picks a voice and language, and downloads a
WAV that contains **real, intelligible speech** (not silence). The same holds for the
inbox watcher.

Success is verified by the **user** running the stack with the model assets fetched and
listening to the output — see [Testing](#testing-strategy). This design cannot be
verified in the authoring environment (no model assets, no ONNX Runtime native library).

## Background

`synthesizeOne` currently returns a zero-filled buffer (silence). The scaffold's comments
assume a single `model.onnx` taking `(tokens, style, lang_id)` and returning a waveform.
**That is wrong.** Investigation of the upstream repo
(`github.com/supertone-inc/supertonic`, `go/` directory) established the real shape.

### Findings (verified against upstream source)

- The upstream `go/` package is small and self-contained: `helper.go` (~28 KB, the whole
  engine) + `example_onnx.go` (CLI wrapper). Module `supertonic-tts`, Go 1.21.
- **No phonemizer / espeak.** Tokenization is a flat unicode-codepoint → token-id lookup
  from `unicode_indexer.json`, preceded by `preprocessText` (NFKD normalization, emoji/
  symbol stripping, dash/quote standardization, whitespace collapse, trailing-period
  insertion). Language is applied by wrapping the text as `<lang>text</lang>`.
- **It's a 4-model pipeline**, run from `assets/onnx/`: `text_encoder`,
  `duration_predictor`, `vector_estimator`, `vocoder`. Plus `tts.json` (model config,
  includes `AE.SampleRate`) and `unicode_indexer.json`.
- **Voices** are `assets/voice_styles/M1.json`, `F1.json`, … — JSON tensors
  (`style_ttl` with `Data`/`Dims`/`Type`), **not** `assets/voices/*.bin`.
- Public API: `LoadTextToSpeech(onnxDir, useGPU, cfg)`, `LoadVoiceStyle(paths, …)`,
  `LoadCfgs(…)`, `TextToSpeech.Call(text, lang, style, totalStep, speed)`,
  `TextToSpeech.SampleRate`, `AvailableLangs`.
- Dependencies: `github.com/yalue/onnxruntime_go v1.11.0`, `github.com/go-audio/wav`,
  `github.com/go-audio/audio`, `github.com/mjibson/go-dsp`, `golang.org/x/text`.
- **Native dependency:** ONNX Runtime C library (upstream README pins **v1.16.0**),
  located at runtime via the `ONNXRUNTIME_LIB_PATH` env var. Inference params default to
  `total-step=8`, `speed=1.05`.

## Approach

**Vendor the upstream Go package; do not reimplement it.** The working tensor
orchestration already exists in `helper.go`; hand-porting it would only add bugs. The
alternative (minimal hand-port) was rejected as pure risk for no benefit.

## Components

### A. Vendored engine — `backend/internal/tts/supertonic_native/`

- Copy `helper.go` and the inference logic from `example_onnx.go` (excluding `main` and
  CLI flag parsing). Rename `package main` → `package supertonic_native`. Export the
  symbols listed in Findings.
- Add upstream dependencies to `backend/go.mod`; run `go mod tidy`.
- Include the upstream repo's `LICENSE` in the vendored directory for attribution, plus a
  short `VENDORED.md` noting source repo, commit/tag, and the local modifications
  (package rename, `main` removal, seed hook). **Verify the upstream license permits
  vendoring** before copying — if it is not MIT/Apache/BSD-compatible, stop and raise it.
- **Seed hook.** Upstream `sampleNoisyLatent` hardcodes
  `rand.New(rand.NewSource(time.Now().UnixNano()))`, making output non-deterministic.
  Replace with an injectable seed/RNG that **defaults to time-based** (preserving upstream
  behavior in production) but can be pinned for reproducible evals. Record as a deliberate
  modification in `VENDORED.md`. Required by Tier 1.

### B. `Engine` rewrite — `backend/internal/tts/supertonic.go`

Keep the existing public interface unchanged so `api/` and `audio/` need no edits:
`NewEngine`, `Voices`, `Languages`, `HasVoice`, `HasLang`, `Synthesize`, `Close`.

- `NewEngine(assetsDir)`:
  - Initialize ONNX Runtime once: resolve the library path from `ONNXRUNTIME_LIB_PATH`
    (fall back to `/usr/local/lib/libonnxruntime.so`, matching the Dockerfile ENV),
    `ort.SetSharedLibraryPath(...)`,
    `ort.InitializeEnvironment()`.
  - Load `tts.json` config and `unicode_indexer.json`.
  - Open the 4 ONNX sessions via vendored `LoadTextToSpeech(filepath.Join(assetsDir,
    "onnx"), false, cfg)`.
  - Discover voices from `assets/voice_styles/*.json` (see D).
  - Languages = `AvailableLangs` from the vendored package (replaces the hand-maintained
    list; see D).
- `synthesizeOne(text, voice, lang)`:
  - Lazily load and cache the `VoiceStyle` for `voice` (keyed by name).
  - Call `TextToSpeech.Call(text, lang, style, 8, 1.05)`.
  - Return `(samples []float32, sampleRate int, err)` where `sampleRate` is the config
    value (no longer hardcoded 44100).
  - Continue to hold `e.mu` for the duration (ONNX sessions are not goroutine-safe).
- `Synthesize` keeps wrapping `synthesizeOne` and encoding via `encodeWAV(samples, sr)`.
- `Close`: destroy the 4 sessions and `ort.DestroyEnvironment()`.

### C. Docker / native library — `Dockerfile`

- **Target arch: linux/arm64** (Apple Silicon), per decision.
- Build stage: download the ONNX Runtime **v1.16.0 aarch64** release tarball, extract
  `libonnxruntime.so*`.
- Runtime stage (`debian:bookworm-slim`): copy the `.so` to `/usr/local/lib/`, run
  `ldconfig`, and set `ENV ONNXRUNTIME_LIB_PATH=/usr/local/lib/libonnxruntime.so`.
- `CGO_ENABLED`: keep `0` **(assumption: `yalue/onnxruntime_go` is CGO-free via
  purego/dlopen — believed, not verified)**. If the build fails on a CGO/import-C error,
  switch the build stage to `CGO_ENABLED=1` with `gcc` installed. This contingency is
  called out in the plan.

### D. Asset-layout corrections

- `discoverVoices` scans `assets/voice_styles/*.json`; voice name = filename without
  `.json`. Keep the existing fallback list **only** if the dir is missing.
- Replace the hand-maintained 31-code list in `languages.go` and the duplicated map in
  `api/server.go` with `AvailableLangs` from the vendored package (single source of
  truth). Keep the code→display-name map (UI-only) but drive its keys off `AvailableLangs`.

### E. Documentation

- Remove the "stubbed / returns silence" warnings from `README.md` and `CLAUDE.md`.
- Document the ONNX Runtime native dependency, the real asset layout
  (`onnx/`, `voice_styles/`, `tts.json`, `unicode_indexer.json`), and that
  `fetch-model.sh` must populate them.

## Data flow

`docx.Extract` → paragraphs → for each voice×lang: `Engine.Synthesize(text, voice, lang)`
→ `synthesizeOne` (preprocess → unicode tokenize → text_encoder → duration_predictor →
vector_estimator → vocoder) → `[]float32` → `encodeWAV` → `para_NNN.wav` →
`audio.Concatenate` → `full.wav`. The api/jobs and audio layers are unchanged; they
already read sample rate from WAV headers, so a non-44100 rate flows through correctly.

## Error handling

- `NewEngine` fails fast (process exit) if the ONNX library can't be loaded, the config/
  indexer JSON is missing, or no ONNX sessions open — these are unrecoverable startup
  conditions. Error messages name the missing path and the `ONNXRUNTIME_LIB_PATH` value.
- Per-paragraph synthesis errors propagate up through the existing job machinery and mark
  the job `error` with the paragraph index (already implemented in `jobs.go`).
- Unknown voice/lang is still rejected at submit time by `HasVoice`/`HasLang`.

## Evaluation & testing

The current failure mode is *silent output that passes every check*, so a green build
proves nothing. Evals are defined in three tiers. All model-dependent tiers are **run by
the user** (no model/ONNX runtime in the authoring env); I write them as scripts/tests,
you execute and report.

**Determinism (resolved by investigation):** upstream `sampleNoisyLatent` seeds its
Gaussian noise with `time.Now().UnixNano()` → **non-deterministic across runs**.
Production keeps that (natural variation is fine); the vendored **seed hook** (Component A)
lets evals pin the seed, making the denoising path fully reproducible for exact comparison.

**Plumbing tests (authoring env, no model):**
- `go build ./...`, `go vet ./...`, `gofmt` clean.
- Unit tests: voice discovery from a temp `voice_styles/` dir; languages wired from
  `AvailableLangs`; `encodeWAV` round-trips the configured sample rate.
- **Vendoring integrity:** assert vendored sources match the recorded upstream tag
  (checksums in `VENDORED.md`) modulo the documented diff (package rename, `main` removal,
  seed hook). Catches accidental drift/corruption in the copy.

**Tier 0 — audio sanity + runtime silence guard (user-run, automatable):**
For every produced WAV, assert: RMS energy ≥ floor (non-silent); all samples finite (no
NaN/Inf); clip ratio ≤ threshold; duration within a plausible band of input length;
sample rate == config. Shipped two ways: (a) a **runtime guard** in `synthesizeOne` that
fails the job with a clear error if a paragraph returns silent/degenerate audio (turns the
current silent-failure into a loud one), and (b) a standalone `eval/` tool that scans an
outbox job dir and reports pass/fail per WAV.

**Tier 1 — wrapper parity (user-run, seed-pinned):**
With the seed pinned, compare `Engine.Synthesize(text, voice, lang)` against a direct call
into the vendored package that mimics the upstream CLI for identical inputs; expect
byte-exact (or < 1e-3 RMS) PCM. Because the inference math is vendored verbatim, this
validates the **Engine wrapper** — asset loading, voice-style selection, `<lang>` wrapping,
text preprocessing — not the model math (a source diff already covers that). Includes the
upstream example's default text as an apples-to-apples case.

**Tier 2 — intelligibility round-trip (user-run, heaviest, the real proof):**
An `eval/` script runs generated audio through Whisper ASR and computes WER/CER against
the input over a small fixed eval set: **en + de + ja** (Latin / accented / non-Latin),
each in ≥ 2 voices, plus edge texts (numbers, currency, abbreviations, a long paragraph).
Per-language pass thresholds (e.g. en WER ≤ 0.15) live in the eval config. Only this tier
proves it *says the words* and catches per-language tokenization/normalization breakage.

**Acceptance gate:** "done — verified" requires Tier 0 green on a real run **and** Tier 2
clearing its WER threshold for at least `en`. Tier 1 is a wrapper-debugging aid. Manual
listening is a backstop, not the gate.

**User run sequence:** `./scripts/fetch-model.sh` → `docker compose up --build` → drop a
`.docx`, synthesize → run the `eval/` tools against the outbox → report results/errors
verbatim; we iterate on environment issues.

## Risks & open verification items (first-run candidates)

1. **HF asset layout** may not exactly match `onnx/` + `voice_styles/` + the two JSONs;
   paths may need a tweak once the real tree is seen.
2. **ONNX Runtime `.so` version/arch** compatibility (v1.16.0 aarch64) in the slim image.
3. **CGO-free assumption** for `yalue/onnxruntime_go` (see C contingency).
4. **Upstream code license** must permit vendoring (gate in A).
5. Sample-format mapping (vocoder float32 → int16 PCM) — reuse existing `encodeWAV`
   clamping.

## Out of scope (YAGNI)

- GPU execution (`use-gpu=false` always).
- Exposing `total-step`/`speed` as config (hardcoded 8 / 1.05).
- A production seed knob — the seed hook exists only for reproducible evals; production
  keeps upstream's time-based randomness.
- Batch synthesis (`TextToSpeech.Batch`) — paragraphs run sequentially as today.
- amd64 / multi-arch images (arm64 only for now).
- Streaming/partial-audio responses.
