# Job control: result metadata, stop-and-save, and pause/resume

- **Date:** 2026-05-27
- **Status:** Approved design (spec only — implementation deferred)
- **Scope:** `backend/internal/api` (job orchestrator + HTTP surface) and `frontend/`

## Motivation

Driving a real synthesis run surfaced three gaps. A 2,460-paragraph document
(`Wrong Delivery.docx`) takes ~25 min at ~4.5× realtime, and during that time the
UI shows only a `done/total` counter — you can't see how big the result is, you
can't stop early and keep what's been rendered, and you can't process a chunk,
review it, and decide whether to continue.

Three features address this:

1. **Result metadata** — show the output file's size and "processed" state.
2. **Stop & save** — interrupt a running job and keep what was rendered.
3. **Process X, then pause for more** — render a batch, pause, resume on demand.

#2 and #3 are the same underlying mechanism (decision approved): a **unified
pause/resume model**.

## What we build on (current architecture)

From `backend/internal/api/jobs.go` and `server.go`:

- A **job** = one `.docx` × N voices × M langs → N×M **bundles**, each a directory
  of `para_NNN.wav` files plus a stitched `full.wav`.
- Jobs are **in-memory only** (`JobStore.jobs map[string]*Job`); the on-disk WAVs
  are the source of truth. A process restart orphans in-memory job state but leaves
  the WAVs intact under `job-<uuid>/`. This trade-off is deliberate and **preserved**.
- **One goroutine per job** (`JobStore.run`). Synthesis is **sequential**: bundles
  loop in `runInner`, paragraphs loop in `synthesizeBundle`, each calling
  `engine.Synthesize` (the engine serialises ONNX calls internally behind its own
  mutex).
- All job-state mutation goes through `JobStore.update(id, fn)` under `JobStore.mu`.
  This lock discipline is preserved — new control state uses the same mutex.
- HTTP is gin (`server.go`): `handlers{opts}`, routes registered in `NewServer`.
  `getJob` serialises the `Job` struct directly to JSON, so **new `Job`/`Output`
  fields appear in the API automatically**. CORS already allows `POST`/`OPTIONS`.
- Frontend is vanilla JS (`frontend/app.js`, `index.html`, `styles.css`) — no build
  step. It polls `GET /api/jobs/:id`.

## Feature 1 — result metadata

Pure additive; no lifecycle change.

`Output` (jobs.go:49) gains two fields, set whenever `full.wav` is (re)stitched:

```go
type Output struct {
    Voice       string   `json:"voice"`
    Lang        string   `json:"lang"`
    FullURL     string   `json:"full"`
    Paragraphs  []string `json:"paragraphs"`
    FullBytes   int64    `json:"full_bytes"`    // size of full.wav on disk
    DurationSec float64  `json:"duration_sec"`  // total stitched audio length
}
```

- `FullBytes` from `os.Stat(fullPath).Size()` after `audio.Concatenate`.
- `DurationSec` is the sum of paragraph durations (already computed for
  `manifest.json` via `audio.Duration`); compute once and reuse for both.
- "Processed" needs no new field: a bundle with a populated `FullURL` + non-zero
  `FullBytes` is processed. The UI renders, per bundle: `✓ M1·en — 4.2 MB · 12m 03s`.

## Features 2 + 3 — unified pause/resume

### State machine

Add `StatusPaused JobStatus = "paused"` to the existing
`queued / running / done / error`:

```
queued → running → (paused ⇄ running) → done | error
```

- **Auto-pause (#3):** optional `pause_every` (paragraphs; `0`/absent = off). The
  worker pauses after every X paragraph-synthesis steps.
- **Manual pause (#2):** `POST /api/jobs/:id/pause` requests a pause; the worker
  pauses **after the in-flight paragraph finishes** — a paragraph boundary, so WAV
  files are never truncated.
- **On pause:** the current bundle's `full.wav` is (re)stitched from the paragraphs
  rendered so far → the partial result is saved and downloadable. `status → paused`.
- **Resume:** `POST /api/jobs/:id/resume`. Default continues and **re-applies
  `pause_every`** (renders the next X, then pauses again — literally "process X then
  pause for more"). Body `{"all": true}` runs to completion (auto-pause off for the
  remainder).
- **Finalize:** `POST /api/jobs/:id/finalize` ends the job: the worker finishes the
  in-flight paragraph (if running), stitches the partial, and goes `done` with no
  resume. Accepted from **running or paused**, so "stop & save for good" is one call.

### Worker mechanism — block-in-place (recommended, approved)

The job goroutine stays alive and **blocks on a resume channel** while paused.
Paragraph list and indices stay in memory; no state reconstruction. A blocked
goroutine is cheap and is **not** holding the engine mutex, so other jobs and the
folder watcher keep running. Consistent with the existing "restart orphans
in-memory jobs, on-disk WAVs remain" trade-off.

*Rejected alternative:* exit the goroutine on pause and respawn on resume, rebuilding
the done-set from disk. More moving parts; only wins if paused jobs must survive a
process restart — which the design explicitly does not promise.

### Control state

A per-job control struct, held in `JobStore` (transient, **not** serialised),
guarded by the existing `JobStore.mu`:

```go
type jobControl struct {
    pauseEvery        int            // X; 0 = no auto-pause (mutated to 0 by resume-all)
    pauseRequested    bool           // set by /pause
    finalizeRequested bool           // set by /finalize while running
    resume            chan resumeReq // buffered (cap 1); worker receives while paused
}

type resumeReq struct {
    all      bool // disable auto-pause for the remainder
    finalize bool // end the job instead of continuing
}
```

`JobStore` gains `controls map[string]*jobControl` (created in `Submit`, deleted when
the job reaches `done`/`error`). `Job` gains `PauseEvery int json:"pause_every,omitempty"`
for UI visibility.

### Worker loop (revised `runInner`/`synthesizeBundle`)

The bundle/paragraph loops gain a control check at each **paragraph boundary**, before
synthesizing the next paragraph (pseudocode):

```
stepsSinceResume := 0
for each (voice, lang) bundle:
    ensure a live Output for this bundle (paragraph URLs appended as rendered)
    for each paragraph p:
        // --- control check (under JobStore.mu) ---
        if finalizeRequested:
            stitchPartial(currentBundle); status = done; cleanup; return
        if pauseRequested OR (pauseEvery > 0 AND stepsSinceResume >= pauseEvery):
            stitchPartial(currentBundle)      // save what's processed
            status = paused
            req := <-control.resume           // BLOCK here
            if req.finalize:
                stitchPartial(currentBundle); status = done; cleanup; return
            pauseRequested = false; stepsSinceResume = 0
            if req.all { pauseEvery = 0 }
            status = running
        // --- synthesize ---
        wav := engine.Synthesize(p.Text, voice, lang)
        write para_NNN.wav; append URL to live Output; Progress.Done++; stepsSinceResume++
    finalStitch(currentBundle)                // full.wav from all paragraphs
status = done; cleanup
```

`stitchPartial` re-runs `audio.Concatenate` over the paragraph WAVs rendered so far
for the current bundle and updates that `Output`'s `FullURL` / `FullBytes` /
`DurationSec`. Re-stitching is cheap relative to synthesis. Bundles completed before
the pause already have a final `full.wav` and are untouched.

### Multi-bundle semantics (approved defaults)

- `pause_every` counts **paragraph steps globally** across all voice×lang bundles
  (the existing `Progress.Done`), so a pause can land mid-bundle. The current bundle
  gets a partial `full.wav`; earlier bundles are already complete.
- The live `Output` for the in-progress bundle is maintained as paragraphs render
  (a small change from "append Output only when the bundle completes"), so the UI
  shows partial progress and the partial `full.wav` link while paused.

## API surface

| Method + path | Body / params | From state | Result |
|---|---|---|---|
| `POST /api/synthesize` | + optional `pause_every` (int ≥ 0) | — | unchanged response (`job_id`, `status_url`) |
| `POST /api/jobs/:id/pause` | — | `running` | `200` job; `409` otherwise |
| `POST /api/jobs/:id/resume` | optional `{"all": true}` | `paused` | `200` job; `409` otherwise |
| `POST /api/jobs/:id/finalize` | — | `running`/`paused` | `200` job; `409` if `done`/`error` |
| `GET /api/jobs/:id` | — | any | `status` may be `paused`; `Output` has `full_bytes`, `duration_sec`; `Job` has `pause_every` |

- New routes registered in `NewServer` alongside the existing ones; new `handlers`
  methods (`pause`, `resume`, `finalize`) call new `JobStore` methods
  (`Pause(id)`, `Resume(id, all)`, `Finalize(id)`), keeping orchestration in
  `jobs.go` and HTTP concerns in `server.go`.
- `pause_every` parsed from `c.Request.FormValue("pause_every")`; reject negative.
- State-guard errors return `409 Conflict` with the existing
  `{"error": "..."}` envelope.

## Frontend (`frontend/app.js`)

- Upload form: an optional **"Pause every N paragraphs"** number input → `pause_every`.
- While `running`: a **Stop** button (→ `/pause`) and a **Finalize** button
  (→ `/finalize`, stop & save for good).
- While `paused`: **Resume**, **Finish all** (`/resume {all:true}`), **Finalize**
  buttons; a "paused at N / Total" line; and the partial `full.wav` link.
- Per bundle (any state with a result): `✓ <size> · <duration>` from `full_bytes` /
  `duration_sec`.

## Concurrency & edge cases

- All control flags and the `controls` map are read/written under `JobStore.mu`. The
  worker reads them at the paragraph boundary; the channel send/receive happens
  outside the lock to avoid blocking the API.
- `resume`/`finalize` use a **cap-1 buffered channel** so the API never blocks; the
  state guard (`status == paused`) prevents a stray send from being consumed by a
  later pause.
- **`/resume` or `/finalize` while not yet paused** (pause requested but worker hasn't
  hit the boundary): returns `409` — caller retries once actually paused. Acceptable;
  the pause window is sub-second.
- **Natural completion races a manual pause:** if the job finishes first, `/pause`
  returns `409` (`done`). Fine.
- **Process restart while paused:** in-memory job + control + blocked goroutine are
  lost; the on-disk partial `full.wav` and paragraph WAVs remain (orphaned, harmless)
  — same as today.
- `controls` entry is deleted on `done`/`error` to avoid leaking a channel per job.

## Testing strategy (spec'd; implemented with the feature)

Tests use a **fake `tts.Engine`** (the interface is the seam — no ONNX needed),
returning a fixed tiny WAV quickly, plus a doc fixture of K paragraphs.

- State transitions: `running → paused → running → done`; `running → finalize → done`;
  `paused → finalize → done`.
- Auto-pause fires after exactly `pause_every` paragraph steps (incl. mid-bundle for
  a multi-bundle job).
- Manual pause is honored at the **next paragraph boundary** (the in-flight paragraph
  completes; `Progress.Done` advances by exactly one after the pause request).
- **Partial stitch correctness:** after pausing at K paragraphs, `full.wav` equals the
  concatenation of the first K paragraph WAVs (byte-for-byte against an independent
  stitch).
- Resume continues from the correct index (no re-synthesis, no skipped paragraph).
- Guard errors: `/pause` on a `done` job → `409`; `/resume` on a `running` job → `409`.

## Out of scope (YAGNI)

- Persisting paused jobs across a process restart.
- Pausing mid-paragraph.
- Auto-resume timers / scheduled continuation.
- Per-bundle independent pause counters (global counter only).
- Cancelling the in-flight ONNX call (we finish the current paragraph first).
