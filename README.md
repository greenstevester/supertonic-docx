# supertonic-docx

**Drop a Word document → get multilingual, multi-voice TTS audio.**
Fully offline. One Docker command. Built on [Supertonic 3](https://huggingface.co/Supertone/supertonic-3)
(31 languages, ~99M params, ONNX, CPU-only).

Two ways to use it, both from the same Docker Compose stack:

1. **Web UI** — drag a `.docx` into the browser, pick voices + languages, get download links.
2. **Watched folder** — drop a `.docx` into `./inbox/`, audio appears in `./outbox/`.

> ⚠️ **Read this before you expect audio:** the ONNX inference call is currently **stubbed** —
> it emits *silence*. Everything else (parsing, jobs, stitching, API, UI, Docker) is real and
> works end-to-end, so you can run the whole pipeline today and it'll produce playable WAVs that
> are silent. Wiring in real audio is one function. See **[Current status](#current-status)**.

---

## Prerequisites

You need different tools depending on how you run it. **The Docker path is recommended** — it's
the only thing you have to install besides the model fetcher.

| Tool | Needed for | Why | Check |
|---|---|---|---|
| **Docker** (with Compose v2) | Running the app | Builds + runs the whole stack with one command | `docker compose version` |
| **git** | Cloning + fetching the model | `fetch-model.sh` clones the model repo | `git --version` |
| **git-lfs** | Fetching the model | Supertonic's ONNX weights are stored as Git LFS objects | `git lfs version` |
| **Go 1.22+** | *Local* dev only (running outside Docker) | Building/running the backend directly | `go version` |

Installing **git-lfs** (one-time):

```bash
# macOS
brew install git-lfs && git lfs install

# Debian / Ubuntu
sudo apt-get install git-lfs && git lfs install
```

**Disk:** ~200 MB for the model in `./assets`, plus whatever the generated audio adds up to in
`./outbox`. **Hardware:** CPU-only — no GPU, no CUDA, nothing to configure.

---

## Setup (Docker — recommended)

```bash
# 1. Clone
git clone https://github.com/YOUR-USER/supertonic-docx.git
cd supertonic-docx

# 2. Fetch the model (~200 MB) into ./assets — needs git-lfs (see Prerequisites)
./scripts/fetch-model.sh

# 3. Build and start the stack
docker compose up --build
```

That's the whole setup. You now have:

- **Web UI** → http://localhost:8787
- **Watched folder** → drop `.docx` files into `./inbox/`; output lands in `./outbox/job-<id>/`

To stop: `docker compose down`.

**What "working" looks like right now:** upload a `.docx`, watch the progress bar tick to 100%,
click through to per-paragraph WAVs and a stitched `full.wav`. They play as **silence** — that's
expected until the inference stub is replaced (see [Current status](#current-status)). If you
see silent WAVs, setup is correct.

### Setup (local, without Docker)

For backend development you can skip Docker. You still need the model assets (steps 1–2 above)
and **Go 1.22+**.

The binary's defaults point at in-container paths (`/app/assets`, …), so you **must** override
the env vars to local directories — otherwise it won't find the model:

```bash
cd backend
SUPERTONIC_ASSETS=../assets \
SUPERTONIC_INBOX=../inbox \
SUPERTONIC_OUTBOX=../outbox \
SUPERTONIC_FRONTEND=../frontend \
go run ./cmd/server
```

Build a standalone binary instead: `go build -o server ./cmd/server`.

---

## Architecture

One Go binary serves three things from a single process: the web UI, the HTTP API, and the
inbox folder watcher. Which modes are active is driven by environment variables, not flags, so
the same image runs unchanged under Docker Compose.

```
            ┌──────────────┐                         ┌──────────────────┐
  .docx ──▶ │  Web UI       │ ──┐                     │  Supertonic 3    │
            │  (POST /api)  │   │   plain-text        │  (ONNX Runtime,  │
            └──────────────┘   ├── paragraphs ──────▶ │   CPU, in-proc)  │
            ┌──────────────┐   │                      └────────┬─────────┘
  .docx ──▶ │  inbox/       │ ──┘                               │ float32 PCM
            │  watcher      │                                   ▼
            └──────────────┘                          ┌──────────────────┐
                                                       │  per-paragraph   │
                                                       │  WAVs ──stitch──▶│ full.wav
                                                       └────────┬─────────┘
                                                                ▼
                                                       ./outbox/job-<id>/
```

### Pipeline, step by step

1. **Ingest** — a `.docx` arrives via HTTP upload (`POST /api/synthesize`) or by being dropped
   into `inbox/` (the watcher debounces 2s so half-written files aren't grabbed early).
2. **Extract** (`internal/docx`) — a `.docx` is a ZIP; we read `word/document.xml` and stream it
   with stdlib `encoding/xml`, emitting one **plain-text** paragraph per `<w:p>`. No markdown, no
   pandoc, no formatting preserved — TTS only needs linear text. Empty/whitespace paragraphs are
   dropped.
3. **Synthesize** (`internal/tts`) — for every **voice × language** combination, each paragraph
   is turned into a WAV by the shared `Engine`.
4. **Stitch** (`internal/audio`) — per-paragraph WAVs are concatenated into one `full.wav` per
   bundle, with a configurable silence gap. Done in-process via header rewrite (all inputs share
   our exact format), so ffmpeg is *not* a runtime dependency.
5. **Serve** — outputs are written under `./outbox/job-<id>/` and served read-only at `/files/…`;
   the UI polls job status and builds download links.

### Key design decisions

- **The Engine seam.** Everything Supertonic/ONNX-specific is isolated in
  `internal/tts/supertonic.go`. The `api`, `docx`, and `audio` packages depend on the
  `tts.Engine` type, never on ONNX directly. This is deliberate: you can swap the inference
  backend (real ONNX, Python SDK over gRPC, MNN for embedded) by editing one file. **This is also
  the single stubbed seam** — see [Current status](#current-status).
- **Jobs live in memory; disk is the source of truth.** A job is `1 docx × N voices × M langs`,
  producing N×M output bundles. Job state is held in RAM only; the WAVs on disk are authoritative.
  A restart mid-job orphans some half-written files harmlessly (all under a `job-<id>/` dir). No
  database — overkill for a single-user/single-machine tool.
- **One goroutine per job, sequential inside.** Synthesis within a job runs one paragraph at a
  time because the ONNX session is CPU-bound and not goroutine-safe; the engine serialises calls
  internally. Bundles are appended to the job record as they finish, so the UI shows partial
  progress.

### Outputs on disk

For each voice × language requested:

```
outbox/job-<id>/
  source.json         ← job-level manifest: every paragraph's index + source text
  M1_en/
    para_001.wav
    para_002.wav
    para_003.wav
    full.wav
    manifest.json     ← this bundle's paragraphs: source text + duration
  F1_de/
    para_001.wav
    ...
    full.wav
    manifest.json
```

The per-bundle `manifest.json` is handy for downstream pipelines (subtitles, alignment, retrying
failed paragraphs); `source.json` captures the document text once per job.

### Inputs

- **Format:** `.docx` only (Word). Other formats can be added by extending `internal/docx/`.
- **Chunking:** split on paragraph boundaries; empty/whitespace-only paragraphs are skipped. Each
  non-empty paragraph becomes one WAV.

---

## Configuration

Environment variables (set in `docker-compose.yml`, or exported for a local run):

| Var | Default | Meaning |
|---|---|---|
| `SUPERTONIC_ASSETS` | `/app/assets` | Path to ONNX model + voice presets |
| `SUPERTONIC_INBOX`  | `/app/inbox`  | Watched folder (set to empty string to disable the watcher) |
| `SUPERTONIC_OUTBOX` | `/app/outbox` | Where audio is written |
| `SUPERTONIC_FRONTEND` | `/app/frontend` | Static web-UI files |
| `SUPERTONIC_PORT`   | `8080`        | HTTP port for the web UI + API |
| `SUPERTONIC_DEFAULT_VOICES` | `M1,F1` | Voices used for watched-folder mode |
| `SUPERTONIC_DEFAULT_LANGS`  | `en`    | Languages used for watched-folder mode |
| `SUPERTONIC_PARA_GAP_MS` | `300` | Silence (ms) between paragraphs in stitched output |

---

## HTTP API

The web UI is a thin shell over this API — useful if you want to script it.

### `GET /api/catalogue`

Lists available voices and languages (used to populate the UI):

```json
{ "voices": ["F1", "M1", ...], "languages": [{ "code": "en", "name": "English" }, ...] }
```

### `POST /api/synthesize`

Multipart form:

| Field | Type | Required | Notes |
|---|---|---|---|
| `file` | `.docx` upload | yes | |
| `voices` | comma-separated | yes | e.g. `M1,F1,M2` |
| `langs` | comma-separated | yes | e.g. `en,de,ja` |

Response (`202 Accepted`):

```json
{ "job_id": "8f3...", "status_url": "/api/jobs/8f3..." }
```

### `GET /api/jobs/{id}`

```json
{
  "id": "8f3...",
  "status": "queued | running | done | error",
  "progress": { "done": 12, "total": 30 },
  "outputs": [
    { "voice": "M1", "lang": "en", "full": "/files/job-8f3.../M1_en/full.wav",
      "paragraphs": ["/files/.../para_001.wav", "..."] }
  ],
  "error": null
}
```

### `GET /files/...`

Static, read-only serving of `./outbox` (path-traversal guarded). The UI builds download links
from this.

---

## Voices & languages

- **Voices:** Supertonic 3 ships preset styles `M1 M2 M3 M4 M5 F1 F2 F3 F4 F5`. The actual set is
  discovered at runtime from `assets/voices/` — drop new voice files in and they appear, no code
  change. (If assets are missing, the engine falls back to the preset list so the UI still loads.)
- **Languages:** 31 codes baked into the model — see `backend/internal/tts/languages.go` for the
  canonical list, or the [model card](https://huggingface.co/Supertone/supertonic-3#supported-languages).

---

## Current status

This repo is a complete, working scaffold of the **pipeline**: docx parsing, paragraph chunking,
job orchestration, WAV stitching, the HTTP API, the folder watcher, the web UI, and the Docker
packaging are all real.

**The ONNX inference call itself is stubbed.** `backend/internal/tts/supertonic.go` contains a
`synthesizeOne` function that currently returns silence with a fake duration proportional to the
input text length. This lets you build, run, and exercise the end-to-end pipeline today — drop a
docx in, watch the progress bar tick, click through to per-paragraph WAVs (which play as silence).

To wire in real audio, that one function needs to call into the upstream Supertonic Go example
([github.com/supertone-inc/supertonic/go](https://github.com/supertone-inc/supertonic/tree/main/go)),
which uses [`onnxruntime_go`](https://github.com/yalue/onnxruntime_go). The `SUPERTONIC:` TODO
comments in `supertonic.go` describe exactly what goes where — a few dozen lines of session setup
and a tensor unpack. The seam is deliberate so you can swap implementations without touching the
rest of the code. Once that's in, the rest of the system Just Works.

---

## Repo layout

```
backend/             Go service (Gin HTTP + folder watcher + TTS pipeline)
  cmd/server/        main.go (config + wiring) + watcher.go (inbox watcher)
  internal/api/      HTTP handlers + in-memory job orchestrator
  internal/docx/     .docx → []Paragraph (plain text)
  internal/tts/      Supertonic Engine (the stubbed ONNX seam) + language catalogue
  internal/audio/    WAV concatenation + silence insertion
frontend/            Static drop-zone UI (no build step, vanilla JS)
scripts/             fetch-model.sh (pulls the model from Hugging Face)
assets/              Supertonic 3 ONNX + voice presets (gitignored — fetched)
inbox/               Drop .docx here for watched-folder mode
outbox/              Generated audio
docker-compose.yml
Dockerfile
```

---

## License

MIT for this repo's code. The Supertonic model itself is OpenRAIL-M — see the model card for use
restrictions.
