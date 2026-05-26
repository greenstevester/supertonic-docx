# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## The one thing to know first

**TTS inference is stubbed.** `backend/internal/tts/supertonic.go` → `synthesizeOne()`
returns *silence* (a zero-filled PCM buffer whose length is faked from the text length).
Every other part of the system is real and runs end-to-end — docx parsing, paragraph
chunking, job orchestration, WAV stitching, the HTTP API, the folder watcher, the web UI,
Docker packaging — so you can drop a `.docx` in and watch the whole pipeline work, but the
audio plays as silence until that one function is wired to the real ONNX session. The doc
comment at the top of `supertonic.go` (and the `SUPERTONIC:` TODO markers inside it) describe
exactly what to replace and the expected tensor shapes.

## Commands

```bash
# Fetch model assets (~200MB, git-lfs) into ./assets — needed before real inference
./scripts/fetch-model.sh

# Full stack (web UI + API + watcher) the way it's meant to run
docker compose up --build      # → http://localhost:8787
docker compose down

# Backend, from ./backend
go build -o server ./cmd/server
go vet ./...
gofmt -l .                     # list unformatted files (none should print)
go test ./...                  # no tests exist yet; this is the command to use when adding them
```

There is no frontend build step — `frontend/` is vanilla JS/HTML/CSS served as static files.
There is no configured linter beyond the Go toolchain (`go vet`, `gofmt`).

### Running the backend outside Docker

The binary's env-var defaults point at container paths (`/app/assets`, `/app/inbox`, …), so a
bare `go run ./cmd/server` will fail to find assets. Override them to local dirs:

```bash
SUPERTONIC_ASSETS=./assets SUPERTONIC_INBOX=./inbox SUPERTONIC_OUTBOX=./outbox \
SUPERTONIC_FRONTEND=./frontend go run ./cmd/server
```

Set `SUPERTONIC_INBOX=""` to disable the watcher (web-UI-only mode). Other knobs:
`SUPERTONIC_PORT`, `SUPERTONIC_PARA_GAP_MS`, `SUPERTONIC_DEFAULT_VOICES`,
`SUPERTONIC_DEFAULT_LANGS` (the last two are watcher-mode defaults only).

## Architecture

One Go binary (`backend/cmd/server`), two entry modes sharing one `tts.Engine`:

- **HTTP/Web UI** (`internal/api/server.go`) — `POST /api/synthesize` (multipart docx upload)
  creates a job; `GET /api/jobs/:id` polls status; `GET /api/catalogue` lists voices/langs;
  `GET /files/*` serves generated audio out of the outbox.
- **Folder watcher** (`cmd/server/watcher.go`) — fsnotify on `inbox/`, with a 2s debounce
  (sync tools touch files repeatedly), submitting each new `.docx` with the configured default
  voices/langs.

Data flow: `docx.Extract` → `[]Paragraph` → for each voice×lang, `tts.Engine.Synthesize` per
paragraph → write `para_NNN.wav` → `audio.Concatenate` into `full.wav`.

### The Engine seam (most important design decision)

All Supertonic/ONNX-specific code is isolated in `internal/tts/supertonic.go`. The `api`,
`audio`, and `docx` packages depend on the `tts.Engine` interface, never on ONNX directly.
This is deliberate so the inference backend can be swapped (real ONNX, Python SDK over gRPC,
MNN for embedded) by changing one file. When wiring real inference, keep the seam — don't
leak ONNX types into the other packages.

The engine is loaded **once** at startup and shared across all HTTP requests and watcher
events. ONNX sessions aren't goroutine-safe, so `Engine.Synthesize` serialises calls behind
`e.mu`.

### Job model (`internal/api/jobs.go`)

- A job = one docx × N voices × M langs, producing N×M output bundles.
- Jobs live **in memory only**; the on-disk WAVs in the outbox are the source of truth. A
  restart orphans half-finished jobs harmlessly (everything is under a `job-<id>/` dir). This
  is intentional — no DB for a single-user/single-machine tool.
- One goroutine per job. Synthesis within a job is **sequential** (CPU-bound, single ONNX
  session). The `Outputs` slice is appended per-bundle so the UI shows partial progress as
  bundles complete.

### Output layout on disk

```
outbox/job-<uuid>/
  source.json                 ← job-level manifest: every paragraph's index + text
  <voice>_<lang>/             ← one dir per bundle, e.g. M1_en/
    para_001.wav  para_002.wav  ...
    full.wav                  ← stitched, with SUPERTONIC_PARA_GAP_MS silence between paras
    manifest.json             ← per-paragraph text + duration (non-fatal if it fails)
```

(Note: the README's "How it works" section shows `outbox/<document-name>/`, but the code
actually keys output dirs by `job-<uuid>`.)

### Voices vs. languages

- **Voices** are discovered at runtime from `assets/voices/*.{bin,npy,onnx}`
  (`Engine.discoverVoices`). If the assets dir is missing, it falls back to a hardcoded
  `M1–M5, F1–F5` so the UI is usable before assets are fetched.
- **Languages** are a hardcoded list of 31 ISO codes in `internal/tts/languages.go` (baked in
  so the binary is self-contained). There's also a duplicated code→name table in
  `api/server.go` (`catalogueLangs`) — kept inline there to dodge an import cycle, so if you
  add a language, update **both** `languages.go` and that map.

## Repo-specific gotchas

- **No pandoc, no ffmpeg in the pipeline.** `docx/extract.go` parses `word/document.xml`
  directly with `encoding/xml` (stdlib only). `audio/stitch.go` concatenates WAVs in-process
  by rewriting the 44-byte header — it assumes a *canonical* PCM WAV layout (fmt chunk size 16,
  data chunk at offset 36) because every input comes from our own encoder. Third-party WAVs
  would need a real chunk walker. ffmpeg is installed in the image only for users' optional
  post-processing, not used by the code. (The README's diagram mentions pandoc; ignore it.)
- **Module path is a placeholder.** `go.mod` declares `github.com/yourorg/supertonic-docx`;
  the README clone URL has `YOUR-USER`. Rename consistently if this gets a real home.
- **`/files/` has a path-traversal guard** (`serveFile` in `server.go`) — keep it if you touch
  static serving.
- **WAV format is 16-bit PCM, mono, 44.1 kHz** throughout (`encodeWAV` in `supertonic.go`).
  `Concatenate` rejects format mismatches, so changing the sample rate/channels means changing
  it in one place and everything downstream follows.
