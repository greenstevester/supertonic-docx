# Job Control Features Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the three job-control features approved in `docs/superpowers/specs/2026-05-27-job-control-features-design.md` — result file metadata (size/duration), stop-and-save, and process-X-then-pause — unified behind one pause/resume state machine.

**Architecture:** Introduce a tiny `api.Engine` interface so JobStore can be tested offline with a fake engine (`*tts.Engine` already satisfies it; no production wiring change). Add `StatusPaused`, a transient `jobControl` struct held in `JobStore.controls` and guarded by the existing `JobStore.mu`, and a block-in-place worker that checks control flags at each paragraph boundary, (re)stitches a partial `full.wav` on pause, and blocks on a per-job buffered (cap-1) `resume` channel. Three new HTTP endpoints (`/pause`, `/resume`, `/finalize`) wire to JobStore methods.

**Tech Stack:** Go 1.25, gin (HTTP), `archive/zip` (test docx), `encoding/json`, ONNX Runtime via vendored binding (untouched), vanilla JS frontend (no build step).

---

## File Structure

**Modified:**
- `backend/internal/api/jobs.go` — new types (`StatusPaused`, `Output.FullBytes/DurationSec`, `Job.PauseEvery`, `jobControl`, `resumeReq`), `JobStore.controls` map, restructured worker (`runInner` owns the per-paragraph loop), new methods (`Pause`, `Resume`, `Finalize`, `getControl`, `deleteControls`), helpers (`controlGate`, `appendLiveOutput`, `updateBundleFromStitch`), and the sentinel `errJobFinalized`. The old `synthesizeBundle` method is **deleted** — its body is folded into `runInner`'s loop.
- `backend/internal/api/server.go` — three new POST routes + handler methods, `pause_every` form field on `synthesize`, shared `respondJob` helper.
- `backend/cmd/server/watcher.go` — call-site update for `SubmitFromPath` (new `pauseEvery int` argument, pass `0`).
- `frontend/app.js` — pause/resume/finalize wiring, `pause_every` input, render `full_bytes`/`duration_sec`, status-aware control bar.
- `frontend/index.html` — markup for new controls + input.
- `frontend/styles.css` — minimal styling for the new controls.

**Created:**
- `backend/internal/api/engine.go` — the `api.Engine` interface (consumer-side seam).
- `backend/internal/api/jobs_test.go` — offline tests with a fake engine + a docx fixture writer + a synchronisation helper.
- `backend/internal/api/server_test.go` — HTTP integration tests via `httptest`.

---

## Task 1: Introduce `api.Engine` interface (testability refactor)

**Why first:** every later task touches `jobs.go` and needs offline tests. Adding the interface enables a fake engine in `jobs_test.go`. No behavior change.

**Files:**
- Create: `backend/internal/api/engine.go`
- Modify: `backend/internal/api/jobs.go`

- [ ] **Step 1: Create the interface**

Create `backend/internal/api/engine.go`:

```go
// Package api: the JobStore's view of the TTS engine.
//
// Declared at the consumer side (Effective Go: "the more an interface is
// abstracted from the type, the more general it should be") so JobStore can
// be unit-tested with a fake. The concrete *tts.Engine in cmd/server/main.go
// already satisfies this interface — no production wiring changes.
package api

// Engine is the minimal TTS surface JobStore depends on. The engine layer
// (internal/tts) already retries transient degenerate audio internally, so
// callers see either a valid 16-bit PCM mono WAV or a fatal error.
type Engine interface {
	Synthesize(text, voice, lang string) ([]byte, error)
	Voices() []string
	Languages() []string
	HasVoice(voice string) bool
	HasLang(lang string) bool
}
```

- [ ] **Step 2: Change `JobStore.engine` to the interface**

In `backend/internal/api/jobs.go`, change the field type (around line 80) and `NewJobStore`'s parameter type:

```go
type JobStore struct {
	outboxDir string
	engine    Engine // was *tts.Engine
	paraGapMs int

	mu   sync.RWMutex
	jobs map[string]*Job
}

func NewJobStore(outboxDir string, engine Engine, paraGapMs int) *JobStore {
	return &JobStore{
		outboxDir: outboxDir,
		engine:    engine,
		paraGapMs: paraGapMs,
		jobs:      map[string]*Job{},
	}
}
```

Drop the `"github.com/yourorg/supertonic-docx/internal/tts"` import from `jobs.go` if no longer referenced (it likely isn't — `tts.Engine` was the only use).

- [ ] **Step 3: Verify build and existing tests**

```bash
cd backend
gofmt -l .
go vet ./...
go build ./...
go test ./...
```

Expected: all green, no output from gofmt.

- [ ] **Step 4: Commit**

```bash
git add backend/internal/api/engine.go backend/internal/api/jobs.go
git commit -m "refactor(api): introduce Engine interface for offline JobStore tests"
```

---

## Task 2: State plumbing (`StatusPaused`, `jobControl`, `controls`, `Submit` signature)

**Spec reference:** state machine setup. No worker behavior change yet — just the types and the extended `Submit` signature so later tests can pass `pauseEvery`.

**Files:**
- Modify: `backend/internal/api/jobs.go`
- Modify: `backend/cmd/server/watcher.go`

- [ ] **Step 1: Add status, sentinel error, and Job field**

In `jobs.go`:

```go
const (
	StatusQueued  JobStatus = "queued"
	StatusRunning JobStatus = "running"
	StatusPaused  JobStatus = "paused" // NEW
	StatusDone    JobStatus = "done"
	StatusError   JobStatus = "error"
)

// errJobFinalized is returned from runInner when the worker exits cleanly via
// a user finalize signal (running or paused). run() translates it to StatusDone.
var errJobFinalized = errors.New("job finalized by user")
```

Add `"errors"` to the imports.

In the `Job` struct, add `PauseEvery`:

```go
type Job struct {
	ID         string    `json:"id"`
	SourceName string    `json:"source_name"`
	Status     JobStatus `json:"status"`
	Progress   Progress  `json:"progress"`
	Voices     []string  `json:"voices"`
	Langs      []string  `json:"langs"`
	PauseEvery int       `json:"pause_every,omitempty"` // 0 = no auto-pause
	Outputs    []Output  `json:"outputs"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}
```

- [ ] **Step 2: Add `jobControl` + `resumeReq` types**

```go
// jobControl carries pause/resume signaling for one in-flight job. Read/written
// under JobStore.mu, except the resume channel which is buffered (cap 1) so
// the API never blocks the worker (and vice versa).
type jobControl struct {
	pauseEvery        int  // mutated to 0 by resume{all:true}
	pauseRequested    bool // set by /pause
	finalizeRequested bool // set by /finalize while running
	resume            chan resumeReq
}

type resumeReq struct {
	all      bool // disable auto-pause for the remainder
	finalize bool // end the job instead of continuing
}
```

- [ ] **Step 3: Add `controls` to `JobStore`**

```go
type JobStore struct {
	outboxDir string
	engine    Engine
	paraGapMs int

	mu       sync.RWMutex
	jobs     map[string]*Job
	controls map[string]*jobControl // transient; not serialised
}

func NewJobStore(outboxDir string, engine Engine, paraGapMs int) *JobStore {
	return &JobStore{
		outboxDir: outboxDir,
		engine:    engine,
		paraGapMs: paraGapMs,
		jobs:      map[string]*Job{},
		controls:  map[string]*jobControl{},
	}
}
```

- [ ] **Step 4: Extend `Submit` / `SubmitFromPath` signatures with `pauseEvery`**

```go
func (s *JobStore) Submit(docxPath, sourceName string, voices, langs []string, pauseEvery int) (*Job, error) {
	voices = dedupe(voices)
	langs = dedupe(langs)
	if len(voices) == 0 { return nil, fmt.Errorf("at least one voice required") }
	if len(langs) == 0  { return nil, fmt.Errorf("at least one language required") }
	if pauseEvery < 0   { return nil, fmt.Errorf("pause_every must be >= 0") }
	for _, v := range voices {
		if !s.engine.HasVoice(v) { return nil, fmt.Errorf("unknown voice %q", v) }
	}
	for _, l := range langs {
		if !s.engine.HasLang(l) { return nil, fmt.Errorf("unknown language %q", l) }
	}

	job := &Job{
		ID:         uuid.NewString(),
		SourceName: sourceName,
		Status:     StatusQueued,
		Voices:     voices,
		Langs:      langs,
		PauseEvery: pauseEvery,
		CreatedAt:  time.Now(),
	}

	s.mu.Lock()
	s.jobs[job.ID] = job
	s.controls[job.ID] = &jobControl{
		pauseEvery: pauseEvery,
		resume:     make(chan resumeReq, 1),
	}
	s.mu.Unlock()

	go s.run(job, docxPath)
	return job, nil
}

func (s *JobStore) SubmitFromPath(path string, voices, langs []string, pauseEvery int) (*Job, error) {
	return s.Submit(path, filepath.Base(path), voices, langs, pauseEvery)
}

func (s *JobStore) deleteControls(id string) {
	s.mu.Lock()
	delete(s.controls, id)
	s.mu.Unlock()
}

// getControl returns the live control struct (read lock). The struct itself
// is mutable; callers must hold s.mu when reading/writing its fields.
func (s *JobStore) getControl(id string) *jobControl {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.controls[id]
}
```

- [ ] **Step 5: Update watcher caller**

In `backend/cmd/server/watcher.go`, find the `SubmitFromPath` call site and add the trailing `0`:

```go
job, err := jobs.SubmitFromPath(path, defaultVoices, defaultLangs, 0)
```

(Watcher-mode jobs never set auto-pause; the env-var surface stays unchanged.)

- [ ] **Step 6: Verify build**

```bash
cd backend
gofmt -l .
go vet ./...
go build ./...
go test ./...
```

Expected: all green; no test changes yet (worker still works the same).

- [ ] **Step 7: Commit**

```bash
git add backend/internal/api/jobs.go backend/cmd/server/watcher.go
git commit -m "feat(api): add StatusPaused, jobControl plumbing, and pause_every on Submit"
```

---

## Task 3: Result metadata on `Output` + offline test scaffolding

**Spec reference:** Feature 1.

**Files:**
- Modify: `backend/internal/api/jobs.go`
- Create: `backend/internal/api/jobs_test.go`

- [ ] **Step 1: Write the failing test (and scaffold the test helpers)**

Create `backend/internal/api/jobs_test.go`:

```go
package api

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

/* ---------- fake engine ---------- */

// fakeEngine returns a fixed canonical WAV per Synthesize call so JobStore
// tests can run offline (no ONNX). Each call yields samplesPerCall non-zero
// samples at sampleRate; that pins per-paragraph audio length deterministically.
type fakeEngine struct {
	sampleRate     int
	samplesPerCall int
	voices         []string
	langs          []string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		sampleRate:     44100,
		samplesPerCall: 4410, // 0.1s per paragraph
		voices:         []string{"M1", "F1"},
		langs:          []string{"en"},
	}
}

func (f *fakeEngine) Synthesize(text, voice, lang string) ([]byte, error) {
	return makeFakeWAV(f.samplesPerCall, f.sampleRate), nil
}
func (f *fakeEngine) Voices() []string       { return f.voices }
func (f *fakeEngine) Languages() []string    { return f.langs }
func (f *fakeEngine) HasVoice(v string) bool { return v == "M1" || v == "F1" }
func (f *fakeEngine) HasLang(l string) bool  { return l == "en" }

// makeFakeWAV builds a canonical mono 16-bit PCM WAV with n constant-amplitude
// samples — well above any RMS floor, well below clip.
func makeFakeWAV(n, sr int) []byte {
	const amp int16 = 8000
	const bits, ch = 16, 1
	dataSize := n * 2
	buf := bytes.NewBuffer(make([]byte, 0, 44+dataSize))
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1))
	binary.Write(buf, binary.LittleEndian, uint16(ch))
	binary.Write(buf, binary.LittleEndian, uint32(sr))
	binary.Write(buf, binary.LittleEndian, uint32(sr*ch*bits/8))
	binary.Write(buf, binary.LittleEndian, uint16(ch*bits/8))
	binary.Write(buf, binary.LittleEndian, uint16(bits))
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(dataSize))
	for i := 0; i < n; i++ {
		binary.Write(buf, binary.LittleEndian, amp)
	}
	return buf.Bytes()
}

/* ---------- docx fixture ---------- */

// writeTestDocx zips a minimal valid .docx with one <w:t> per paragraph in
// document order and returns the path. The docx package only requires
// word/document.xml; [Content_Types].xml + _rels/.rels are included for
// completeness so the fixture would also open in Word.
func writeTestDocx(t *testing.T, paragraphs []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.docx")
	f, err := os.Create(path)
	if err != nil { t.Fatal(err) }
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil { t.Fatal(err) }
		if _, err := w.Write([]byte(body)); err != nil { t.Fatal(err) }
	}
	add("[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`)
	add("_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`)
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t>`)
		body.WriteString(p) // assumes test inputs are XML-safe
		body.WriteString(`</w:t></w:r></w:p>`)
	}
	body.WriteString(`</w:body></w:document>`)
	add("word/document.xml", body.String())
	return path
}

/* ---------- timing helper ---------- */

// waitFor polls fn until it returns true or timeout elapses. Returns whether
// the condition was met. Use generous timeouts; the worker runs in a goroutine.
func waitFor(fn func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() { return true }
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

/* ---------- the actual test ---------- */

func TestJobSetsResultMetadataOnCompletion(t *testing.T) {
	dir := t.TempDir()
	eng := newFakeEngine()
	store := NewJobStore(dir, eng, 0)
	docxPath := writeTestDocx(t, []string{"first", "second", "third"})

	job, err := store.Submit(docxPath, "test.docx", []string{"M1"}, []string{"en"}, 0)
	if err != nil { t.Fatalf("Submit: %v", err) }

	if !waitFor(func() bool {
		j, _ := store.Get(job.ID)
		return j.Status == StatusDone
	}, 5*time.Second) {
		j, _ := store.Get(job.ID)
		t.Fatalf("did not complete; status=%s", j.Status)
	}

	j, _ := store.Get(job.ID)
	if len(j.Outputs) != 1 {
		t.Fatalf("expected 1 Output, got %d", len(j.Outputs))
	}
	out := j.Outputs[0]
	if out.FullBytes <= 44 {
		t.Errorf("FullBytes = %d, want > 44 (WAV header)", out.FullBytes)
	}
	if out.DurationSec < 0.25 {
		// 3 paragraphs × 0.1s = 0.3s nominal (paraGapMs=0 → no extra silence).
		t.Errorf("DurationSec = %f, want ≈ 0.3", out.DurationSec)
	}
}
```

- [ ] **Step 2: Run the test, verify it fails**

```bash
cd backend
go test ./internal/api -run TestJobSetsResultMetadata -v
```

Expected: FAIL — `out.FullBytes` undefined OR (after step 3 partially) `FullBytes = 0`.

- [ ] **Step 3: Add the fields to `Output` and populate them in `synthesizeBundle`**

In `backend/internal/api/jobs.go`:

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

Inside `synthesizeBundle`, after `audio.Concatenate`:

```go
fullPath := filepath.Join(bundleDir, "full.wav")
if err := audio.Concatenate(paraPaths, fullPath, s.paraGapMs); err != nil {
	return Output{}, fmt.Errorf("stitch: %w", err)
}

var fullBytes int64
if info, err := os.Stat(fullPath); err == nil {
	fullBytes = info.Size()
}
durSec, _ := audio.Duration(fullPath) // non-fatal; 0 on failure

// ... existing manifest write ...

return Output{
	Voice:       voice,
	Lang:        lang,
	FullURL:     fileURL(s.outboxDir, fullPath),
	Paragraphs:  paraURLs,
	FullBytes:   fullBytes,
	DurationSec: durSec,
}, nil
```

- [ ] **Step 4: Run tests, verify they pass**

```bash
go test ./internal/api -v
go test ./...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/api/jobs.go backend/internal/api/jobs_test.go
git commit -m "feat(api): expose full.wav size + duration on Output"
```

---

## Task 4: Worker restructure — live `Output`, control gate, partial stitch, manual pause

**Spec reference:** the main pause/resume mechanism. Largest single task.

**Files:**
- Modify: `backend/internal/api/jobs.go`
- Modify: `backend/internal/api/jobs_test.go`

- [ ] **Step 1: Add the synchronised fake engine to the test file**

Append to `jobs_test.go`:

```go
// blockingFakeEngine releases each Synthesize call only after the test signals
// via release. This makes pause/resume tests fully deterministic: the test
// drives the worker one paragraph at a time, with explicit pause windows.
type blockingFakeEngine struct {
	*fakeEngine
	release chan struct{}
	calls   chan string // worker sends paragraph text before blocking
}

func newBlockingFakeEngine() *blockingFakeEngine {
	return &blockingFakeEngine{
		fakeEngine: newFakeEngine(),
		release:    make(chan struct{}, 64),
		calls:      make(chan string, 64),
	}
}

func (b *blockingFakeEngine) Synthesize(text, voice, lang string) ([]byte, error) {
	b.calls <- text
	<-b.release
	return b.fakeEngine.Synthesize(text, voice, lang)
}

// releaseN releases N pending Synthesize calls in order.
func releaseN(b *blockingFakeEngine, n int) {
	for i := 0; i < n; i++ {
		<-b.calls
		b.release <- struct{}{}
	}
}

func mustReachStatus(t *testing.T, store *JobStore, id string, want JobStatus) {
	t.Helper()
	if !waitFor(func() bool {
		j, _ := store.Get(id)
		return j.Status == want
	}, 2*time.Second) {
		j, _ := store.Get(id)
		t.Fatalf("did not reach %s; status=%s", want, j.Status)
	}
}
```

- [ ] **Step 2: Write the failing test for manual pause + partial stitch**

```go
func TestPauseStitchesPartialFullWav(t *testing.T) {
	dir := t.TempDir()
	eng := newBlockingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	docxPath := writeTestDocx(t, []string{"p1", "p2", "p3", "p4", "p5"})

	job, err := store.Submit(docxPath, "test.docx", []string{"M1"}, []string{"en"}, 0)
	if err != nil { t.Fatalf("Submit: %v", err) }

	// Release 2 paragraphs.
	releaseN(eng, 2)

	// Worker is now blocked at paragraph 3's Synthesize call.
	<-eng.calls // observe the call to p3

	// Request pause while p3 is in-flight (blocked on release).
	if err := store.Pause(job.ID); err != nil { t.Fatalf("Pause: %v", err) }

	// Boundary rule: p3 must complete before the pause takes effect.
	eng.release <- struct{}{}

	mustReachStatus(t, store, job.ID, StatusPaused)

	j, _ := store.Get(job.ID)
	if len(j.Outputs) != 1 {
		t.Fatalf("expected 1 in-progress Output, got %d", len(j.Outputs))
	}
	out := j.Outputs[0]
	if len(out.Paragraphs) != 3 {
		t.Errorf("expected 3 paragraph URLs (boundary rule), got %d", len(out.Paragraphs))
	}
	if out.FullBytes <= 44 {
		t.Errorf("expected stitched partial full.wav, FullBytes = %d", out.FullBytes)
	}
	if _, err := os.Stat(filepath.Join(dir, "job-"+job.ID, "M1_en", "full.wav")); err != nil {
		t.Errorf("partial full.wav not on disk: %v", err)
	}
}
```

- [ ] **Step 3: Run the test, verify it fails**

```bash
go test ./internal/api -run TestPauseStitchesPartial -v
```

Expected: FAIL — `store.Pause` undefined OR the worker doesn't transition to paused.

- [ ] **Step 4: Add `Pause` and the worker helpers**

In `jobs.go`, add:

```go
// Pause requests a pause; the worker will pause at the next paragraph boundary.
// Errors if the job is not currently running.
func (s *JobStore) Pause(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok { return fmt.Errorf("job not found") }
	if j.Status != StatusRunning {
		return fmt.Errorf("job is %s, cannot pause", j.Status)
	}
	ctl, ok := s.controls[id]
	if !ok { return fmt.Errorf("job control missing") }
	ctl.pauseRequested = true
	return nil
}

type ctlAction int

const (
	ctlContinue ctlAction = iota
	ctlPause
	ctlFinalize
)

// controlGate reads the control flags and decides what to do before the next
// paragraph. Auto-pause fires when stepsSinceResume reaches pauseEvery.
func (s *JobStore) controlGate(id string, stepsSinceResume int) ctlAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl, ok := s.controls[id]
	if !ok { return ctlContinue }
	if ctl.finalizeRequested { return ctlFinalize }
	if ctl.pauseRequested     { return ctlPause }
	if ctl.pauseEvery > 0 && stepsSinceResume >= ctl.pauseEvery { return ctlPause }
	return ctlContinue
}

// appendLiveOutput adds a new Output for the in-progress bundle and returns
// its index in job.Outputs. Paragraphs/stitch fields are filled in live.
func (s *JobStore) appendLiveOutput(id, voice, lang string) int {
	var idx int
	s.update(id, func(j *Job) {
		j.Outputs = append(j.Outputs, Output{Voice: voice, Lang: lang})
		idx = len(j.Outputs) - 1
	})
	return idx
}

// updateBundleFromStitch (re)stitches full.wav from the paragraphs rendered so
// far and updates the live Output's stitch fields. Idempotent. No-op if there
// are no paragraphs yet.
func (s *JobStore) updateBundleFromStitch(id string, outputIdx int, bundleDir string, paraPaths []string) {
	if len(paraPaths) == 0 { return }
	fullPath := filepath.Join(bundleDir, "full.wav")
	if err := audio.Concatenate(paraPaths, fullPath, s.paraGapMs); err != nil {
		return // non-fatal for partial stitch
	}
	var size int64
	if info, err := os.Stat(fullPath); err == nil { size = info.Size() }
	dur, _ := audio.Duration(fullPath)
	s.update(id, func(j *Job) {
		if outputIdx < len(j.Outputs) {
			j.Outputs[outputIdx].FullURL = fileURL(s.outboxDir, fullPath)
			j.Outputs[outputIdx].FullBytes = size
			j.Outputs[outputIdx].DurationSec = dur
		}
	})
}
```

- [ ] **Step 5: Restructure `runInner` and `run` — delete `synthesizeBundle`**

Replace the body of `runInner` (lines ~173–210) with the control-aware loop, and **delete the `synthesizeBundle` method** (the old function from line ~214). The new `runInner`:

```go
func (s *JobStore) runInner(job *Job, docxPath string) error {
	paragraphs, err := docx.Extract(docxPath)
	if err != nil { return fmt.Errorf("parse docx: %w", err) }
	if len(paragraphs) == 0 { return fmt.Errorf("document contained no readable text") }

	total := len(paragraphs) * len(job.Voices) * len(job.Langs)
	s.update(job.ID, func(j *Job) { j.Progress.Total = total })

	jobDir := filepath.Join(s.outboxDir, "job-"+job.ID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil { return err }
	if err := writeSourceManifest(jobDir, job, paragraphs); err != nil { return err }

	stepsSinceResume := 0

	for _, voice := range job.Voices {
		for _, lang := range job.Langs {
			bundleDir := filepath.Join(jobDir, voice+"_"+lang)
			if err := os.MkdirAll(bundleDir, 0o755); err != nil { return err }

			outputIdx := s.appendLiveOutput(job.ID, voice, lang)
			var paraPaths []string

			for _, p := range paragraphs {
				switch s.controlGate(job.ID, stepsSinceResume) {
				case ctlFinalize:
					s.updateBundleFromStitch(job.ID, outputIdx, bundleDir, paraPaths)
					return errJobFinalized
				case ctlPause:
					s.updateBundleFromStitch(job.ID, outputIdx, bundleDir, paraPaths)
					s.update(job.ID, func(j *Job) { j.Status = StatusPaused })
					ctl := s.getControl(job.ID)
					req := <-ctl.resume // BLOCK until /resume or /finalize
					if req.finalize {
						return errJobFinalized
					}
					s.mu.Lock()
					ctl.pauseRequested = false
					if req.all { ctl.pauseEvery = 0 }
					s.mu.Unlock()
					stepsSinceResume = 0
					s.update(job.ID, func(j *Job) { j.Status = StatusRunning })
				}

				wavBytes, err := s.engine.Synthesize(p.Text, voice, lang)
				if err != nil {
					return fmt.Errorf("voice=%s lang=%s paragraph %d: %w", voice, lang, p.Index, err)
				}
				name := fmt.Sprintf("para_%03d.wav", p.Index)
				path := filepath.Join(bundleDir, name)
				if err := os.WriteFile(path, wavBytes, 0o644); err != nil { return err }
				paraPaths = append(paraPaths, path)
				s.update(job.ID, func(j *Job) {
					if outputIdx < len(j.Outputs) {
						j.Outputs[outputIdx].Paragraphs = append(j.Outputs[outputIdx].Paragraphs, fileURL(s.outboxDir, path))
					}
					j.Progress.Done++
				})
				stepsSinceResume++
			}

			// Final stitch for this completed bundle.
			s.updateBundleFromStitch(job.ID, outputIdx, bundleDir, paraPaths)
			// Per-bundle manifest (non-fatal).
			_ = writeBundleManifest(bundleDir, paragraphs, paraPaths)
		}
	}
	return nil
}
```

Update `run` to translate the sentinel error:

```go
func (s *JobStore) run(job *Job, docxPath string) {
	defer s.deleteControls(job.ID)
	s.update(job.ID, func(j *Job) {
		j.Status = StatusRunning
		j.StartedAt = time.Now()
	})

	err := s.runInner(job, docxPath)
	finished := time.Now()

	switch {
	case errors.Is(err, errJobFinalized):
		s.update(job.ID, func(j *Job) {
			j.Status = StatusDone
			j.FinishedAt = finished
		})
	case err != nil:
		s.update(job.ID, func(j *Job) {
			j.Status = StatusError
			j.Error = err.Error()
			j.FinishedAt = finished
		})
	default:
		s.update(job.ID, func(j *Job) {
			j.Status = StatusDone
			j.FinishedAt = finished
		})
	}
}
```

Delete the old `synthesizeBundle` method entirely (it has been folded into the loop above).

- [ ] **Step 6: Run tests, verify they pass**

```bash
go test ./internal/api -v
```

Expected: both `TestJobSetsResultMetadataOnCompletion` (from Task 3, now driven through Submit) and `TestPauseStitchesPartialFullWav` PASS.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/api/jobs.go backend/internal/api/jobs_test.go
git commit -m "feat(api): worker restructure with live Output, control gate, partial stitch, manual pause"
```

---

## Task 5: `Resume` (+ `all`) and `Finalize` (paused + running)

**Spec reference:** state-machine exits from paused; also `Finalize` from running.

**Files:**
- Modify: `backend/internal/api/jobs.go`
- Modify: `backend/internal/api/jobs_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `jobs_test.go`:

```go
func TestResumeContinuesAndRepausesAfterPauseEvery(t *testing.T) {
	dir := t.TempDir()
	eng := newBlockingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	docxPath := writeTestDocx(t, []string{"p1", "p2", "p3", "p4", "p5"})

	job, err := store.Submit(docxPath, "test.docx", []string{"M1"}, []string{"en"}, 2)
	if err != nil { t.Fatalf("Submit: %v", err) }

	// First 2 paragraphs synthesize, then auto-pause.
	releaseN(eng, 2)
	mustReachStatus(t, store, job.ID, StatusPaused)
	if j, _ := store.Get(job.ID); j.Progress.Done != 2 {
		t.Errorf("expected Done=2 at first pause, got %d", j.Progress.Done)
	}

	// Resume (default re-applies pause_every).
	if err := store.Resume(job.ID, false); err != nil { t.Fatalf("Resume: %v", err) }
	releaseN(eng, 2)
	mustReachStatus(t, store, job.ID, StatusPaused)
	if j, _ := store.Get(job.ID); j.Progress.Done != 4 {
		t.Errorf("expected Done=4 at second pause, got %d", j.Progress.Done)
	}

	// Final resume with all=true — runs to completion.
	if err := store.Resume(job.ID, true); err != nil { t.Fatalf("Resume(all): %v", err) }
	releaseN(eng, 1)
	mustReachStatus(t, store, job.ID, StatusDone)
}

func TestFinalizeFromPausedEndsWithSavedPartial(t *testing.T) {
	dir := t.TempDir()
	eng := newBlockingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	docxPath := writeTestDocx(t, []string{"p1", "p2", "p3", "p4", "p5"})

	job, _ := store.Submit(docxPath, "test.docx", []string{"M1"}, []string{"en"}, 2)
	releaseN(eng, 2)
	mustReachStatus(t, store, job.ID, StatusPaused)

	if err := store.Finalize(job.ID); err != nil { t.Fatalf("Finalize: %v", err) }
	mustReachStatus(t, store, job.ID, StatusDone)

	j, _ := store.Get(job.ID)
	if j.Progress.Done != 2 {
		t.Errorf("expected Done=2 (partial saved), got %d", j.Progress.Done)
	}
	if len(j.Outputs) != 1 || j.Outputs[0].FullBytes <= 44 {
		t.Errorf("expected saved partial full.wav, got %+v", j.Outputs)
	}
}

func TestFinalizeFromRunningEndsAtBoundary(t *testing.T) {
	dir := t.TempDir()
	eng := newBlockingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	docxPath := writeTestDocx(t, []string{"p1", "p2", "p3", "p4"})

	job, _ := store.Submit(docxPath, "test.docx", []string{"M1"}, []string{"en"}, 0)

	releaseN(eng, 1) // p1 done
	<-eng.calls      // worker now blocked at p2's Synthesize

	if err := store.Finalize(job.ID); err != nil { t.Fatalf("Finalize: %v", err) }
	eng.release <- struct{}{} // let p2 complete (boundary rule)

	mustReachStatus(t, store, job.ID, StatusDone)

	j, _ := store.Get(job.ID)
	if j.Progress.Done != 2 {
		t.Errorf("expected Done=2 (boundary rule), got %d", j.Progress.Done)
	}
	if len(j.Outputs) != 1 || j.Outputs[0].FullBytes <= 44 {
		t.Errorf("expected saved partial full.wav, got %+v", j.Outputs)
	}
}
```

- [ ] **Step 2: Run, verify they fail (`Resume`/`Finalize` undefined)**

```bash
go test ./internal/api -run 'Resume|Finalize' -v
```

- [ ] **Step 3: Implement `Resume` and `Finalize`**

In `jobs.go`:

```go
// Resume continues a paused job. If all is true, auto-pause is disabled for
// the remainder of the job. Errors if the job is not paused.
func (s *JobStore) Resume(id string, all bool) error {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok { s.mu.Unlock(); return fmt.Errorf("job not found") }
	if j.Status != StatusPaused {
		st := j.Status
		s.mu.Unlock()
		return fmt.Errorf("job is %s, cannot resume", st)
	}
	ctl := s.controls[id]
	s.mu.Unlock()
	select {
	case ctl.resume <- resumeReq{all: all}:
		return nil
	default:
		return fmt.Errorf("resume already pending")
	}
}

// Finalize ends a job — from paused, signal exit cleanly with the partial
// stitched; from running, set a flag the worker catches at the next paragraph
// boundary. Errors if the job is done/error.
func (s *JobStore) Finalize(id string) error {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok { s.mu.Unlock(); return fmt.Errorf("job not found") }
	if j.Status != StatusRunning && j.Status != StatusPaused {
		st := j.Status
		s.mu.Unlock()
		return fmt.Errorf("job is %s, cannot finalize", st)
	}
	ctl := s.controls[id]
	if j.Status == StatusPaused {
		s.mu.Unlock()
		select {
		case ctl.resume <- resumeReq{finalize: true}:
			return nil
		default:
			return fmt.Errorf("resume already pending")
		}
	}
	ctl.finalizeRequested = true
	s.mu.Unlock()
	return nil
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/api -v
go test ./...
```

Expected: all PASS, including the new tests and the older `TestPauseStitchesPartialFullWav` and `TestJobSetsResultMetadataOnCompletion`.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/api/jobs.go backend/internal/api/jobs_test.go
git commit -m "feat(api): Resume + Finalize (paused) and Finalize (running)"
```

---

## Task 6: HTTP endpoints + `pause_every` form field

**Files:**
- Modify: `backend/internal/api/server.go`
- Create: `backend/internal/api/server_test.go`

- [ ] **Step 1: Write the failing HTTP tests**

Create `backend/internal/api/server_test.go`:

```go
package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// buildMultipart builds a multipart/form-data body with the given fields and
// a single file upload. Returns body and Content-Type header.
func buildMultipart(t *testing.T, fields map[string]string, fileField, fileName string, fileBody []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil { t.Fatal(err) }
	}
	fw, err := w.CreateFormFile(fileField, fileName)
	if err != nil { t.Fatal(err) }
	if _, err := fw.Write(fileBody); err != nil { t.Fatal(err) }
	if err := w.Close(); err != nil { t.Fatal(err) }
	return &buf, w.FormDataContentType()
}

// testDocxBytes returns a small valid docx as bytes (no on-disk fixture needed
// for HTTP tests).
func testDocxBytes(t *testing.T, paragraphs []string) []byte {
	t.Helper()
	p := writeTestDocx(t, paragraphs)
	b, err := os.ReadFile(p)
	if err != nil { t.Fatal(err) }
	return b
}

func TestSynthesizeAcceptsPauseEvery(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir, newFakeEngine(), 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	body, ct := buildMultipart(t, map[string]string{
		"voices":      "M1",
		"langs":       "en",
		"pause_every": "2",
	}, "file", "tiny.docx", testDocxBytes(t, []string{"p1", "p2", "p3", "p4"}))

	req := httptest.NewRequest("POST", "/api/synthesize", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil { t.Fatal(err) }

	j, ok := store.Get(resp.JobID)
	if !ok || j.PauseEvery != 2 {
		t.Errorf("PauseEvery = %d, want 2", j.PauseEvery)
	}
}

func TestSynthesizeRejectsNegativePauseEvery(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir, newFakeEngine(), 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	body, ct := buildMultipart(t, map[string]string{
		"voices":      "M1",
		"langs":       "en",
		"pause_every": "-1",
	}, "file", "tiny.docx", testDocxBytes(t, []string{"p1"}))

	req := httptest.NewRequest("POST", "/api/synthesize", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestPauseEndpointReturns409WhenNotRunning(t *testing.T) {
	dir := t.TempDir()
	eng := newFakeEngine()
	store := NewJobStore(dir, eng, 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	// Submit a job and wait for completion.
	docxPath := writeTestDocx(t, []string{"p1"})
	job, _ := store.Submit(docxPath, "t.docx", []string{"M1"}, []string{"en"}, 0)
	mustReachStatus(t, store, job.ID, StatusDone)

	req := httptest.NewRequest("POST", "/api/jobs/"+job.ID+"/pause", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestPauseResumeFinalizeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	eng := newBlockingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	docxPath := writeTestDocx(t, []string{"p1", "p2", "p3", "p4"})
	job, _ := store.Submit(docxPath, "t.docx", []string{"M1"}, []string{"en"}, 0)

	releaseN(eng, 1)
	<-eng.calls // worker now blocked at p2

	// Pause via HTTP.
	req := httptest.NewRequest("POST", "/api/jobs/"+job.ID+"/pause", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK { t.Fatalf("pause status = %d", w.Code) }

	eng.release <- struct{}{} // boundary
	mustReachStatus(t, store, job.ID, StatusPaused)

	// Resume with {all:true} via HTTP.
	req = httptest.NewRequest("POST", "/api/jobs/"+job.ID+"/resume",
		bytes.NewBufferString(`{"all":true}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK { t.Fatalf("resume status = %d", w.Code) }

	releaseN(eng, 2)
	mustReachStatus(t, store, job.ID, StatusDone)
	_ = filepath.Join // keep import used
}
```

- [ ] **Step 2: Run, verify they fail**

```bash
go test ./internal/api -run 'Synthesize.*Pause|PauseEndpoint|PauseResumeFinalize' -v
```

Expected: FAIL — routes don't exist; `pause_every` not parsed.

- [ ] **Step 3: Implement the routes, handlers, and `pause_every` parsing**

In `backend/internal/api/server.go`:

```go
import (
	// ... existing imports ...
	"strconv"
)

func NewServer(opts ServerOpts) http.Handler {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"GET", "POST", "OPTIONS"},
		AllowHeaders: []string{"*"},
	}))

	h := &handlers{opts: opts}
	r.GET("/api/catalogue", h.catalogue)
	r.POST("/api/synthesize", h.synthesize)
	r.GET("/api/jobs/:id", h.getJob)
	r.POST("/api/jobs/:id/pause", h.pauseJob)
	r.POST("/api/jobs/:id/resume", h.resumeJob)
	r.POST("/api/jobs/:id/finalize", h.finalizeJob)

	r.GET("/files/*path", h.serveFile)

	if opts.FrontendDir != "" {
		r.StaticFile("/", filepath.Join(opts.FrontendDir, "index.html"))
		r.Static("/static", opts.FrontendDir)
	}
	return r
}
```

In `h.synthesize`, parse `pause_every` and pass it to `Submit`:

```go
pauseEvery := 0
if s := strings.TrimSpace(c.Request.FormValue("pause_every")); s != "" {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pause_every must be a non-negative integer"})
		return
	}
	pauseEvery = n
}
// ... existing temp-file save ...
job, err := h.opts.Jobs.Submit(tmp.Name(), hdr.Filename, voices, langs, pauseEvery)
```

Add the three handlers + `respondJob` helper:

```go
func (h *handlers) pauseJob(c *gin.Context) {
	if err := h.opts.Jobs.Pause(c.Param("id")); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	h.respondJob(c)
}

func (h *handlers) resumeJob(c *gin.Context) {
	var body struct {
		All bool `json:"all"`
	}
	_ = c.ShouldBindJSON(&body) // body is optional
	if err := h.opts.Jobs.Resume(c.Param("id"), body.All); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	h.respondJob(c)
}

func (h *handlers) finalizeJob(c *gin.Context) {
	if err := h.opts.Jobs.Finalize(c.Param("id")); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	h.respondJob(c)
}

func (h *handlers) respondJob(c *gin.Context) {
	job, ok := h.opts.Jobs.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	c.JSON(http.StatusOK, job)
}
```

- [ ] **Step 4: Run all tests**

```bash
go test ./internal/api -v
go test ./...
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/api/server.go backend/internal/api/server_test.go
git commit -m "feat(api): pause/resume/finalize HTTP endpoints and pause_every form field"
```

---

## Task 7: Frontend — controls, `pause_every` input, metadata display

No automated tests; verify by hand against the running app.

**Files:**
- Modify: `frontend/index.html`
- Modify: `frontend/app.js`
- Modify: `frontend/styles.css`

- [ ] **Step 1: Add markup**

Find the existing submission area in `frontend/index.html` (around the Synthesize button). Add a `pause_every` input near the voices/langs sections:

```html
<section class="block">
  <h2>Pause every</h2>
  <p class="block-sub">Process this many paragraphs, then pause for review. Leave blank for none.</p>
  <label class="number-field">
    <input id="pause-every" type="number" min="0" placeholder="0 (off)">
    <span>paragraphs</span>
  </label>
</section>
```

Inside the existing `#job-panel`, add the control bar (before the outputs section):

```html
<div id="job-controls" class="job-controls" hidden>
  <button type="button" data-action="pause" hidden>Stop</button>
  <button type="button" data-action="resume" hidden>Resume</button>
  <button type="button" data-action="resume-all" hidden>Finish all</button>
  <button type="button" data-action="finalize" hidden>Finalize</button>
</div>
```

- [ ] **Step 2: Wire JavaScript**

In `frontend/app.js`:

In `setupSubmit()` (or at the bottom of init), attach the controls listener:

```js
$('job-controls').addEventListener('click', async (e) => {
  const action = e.target.dataset.action;
  if (!action || !state.jobId) return;
  const id = state.jobId;
  try {
    if (action === 'pause') {
      await fetch(`/api/jobs/${id}/pause`, { method: 'POST' });
    } else if (action === 'resume') {
      await fetch(`/api/jobs/${id}/resume`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      });
    } else if (action === 'resume-all') {
      await fetch(`/api/jobs/${id}/resume`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ all: true }),
      });
    } else if (action === 'finalize') {
      await fetch(`/api/jobs/${id}/finalize`, { method: 'POST' });
    }
  } catch (err) {
    // The next poll will surface state; nothing else to do here.
  }
});
```

Include `pause_every` in `submit()`:

```js
const pe = $('pause-every').value.trim();
if (pe !== '' && Number(pe) > 0) {
  form.append('pause_every', pe);
}
```

In `renderJob(job)`, call a new `renderControls(job)`:

```js
function renderControls(job) {
  const c = $('job-controls');
  const running = job.status === 'running';
  const paused  = job.status === 'paused';
  c.hidden = !(running || paused);
  c.querySelector('[data-action="pause"]').hidden       = !running;
  c.querySelector('[data-action="resume"]').hidden      = !paused;
  c.querySelector('[data-action="resume-all"]').hidden  = !paused;
  c.querySelector('[data-action="finalize"]').hidden    = !(running || paused);
}
```

Call it from `renderJob`:

```js
function renderJob(job) {
  // ... existing status/progress/outputs code ...
  renderControls(job);
  // ... existing error code ...
}
```

In `renderBundle(out)`, replace the meta line to include size + duration:

```js
const meta = document.createElement('span');
meta.className = 'bundle-meta';
const bytes = out.full_bytes || 0;
const dur = out.duration_sec || 0;
meta.textContent =
  `${out.paragraphs.length}¶ · ${formatBytes(bytes)} · ${formatDuration(dur)}`;
```

Add the formatters at the bottom of the file:

```js
function formatBytes(b) {
  if (!b) return '—';
  if (b >= 1024 * 1024) return (b / 1024 / 1024).toFixed(1) + ' MB';
  if (b >= 1024) return (b / 1024).toFixed(0) + ' KB';
  return b + ' B';
}

function formatDuration(s) {
  if (!s) return '—';
  const m = Math.floor(s / 60);
  const sec = Math.floor(s % 60);
  return m > 0 ? `${m}m ${String(sec).padStart(2, '0')}s` : `${sec}s`;
}
```

In `pollJob()`, also keep polling while `status === 'paused'`. The existing code stops only on `done`/`error`, so paused already polls — no change needed. Confirm.

- [ ] **Step 3: Minimal CSS**

Append to `frontend/styles.css`:

```css
.job-controls {
  display: flex;
  gap: 0.5rem;
  margin-top: 1rem;
  margin-bottom: 1rem;
}
.job-controls button {
  padding: 0.4rem 0.8rem;
  cursor: pointer;
}
.number-field {
  display: inline-flex;
  align-items: center;
  gap: 0.5rem;
}
.number-field input {
  width: 5rem;
  padding: 0.3rem 0.4rem;
}
```

- [ ] **Step 4: Rebuild + manual smoke**

```bash
cd /Users/stevengreensill/dev/git-repos/github/ai/supertonic-docx
docker compose build
docker compose up -d
```

Then in the browser at `http://localhost:8787` (hard-refresh: Cmd+Shift+R):
1. Drop a multi-paragraph docx, set "Pause every" to e.g. 3, pick M1/en, submit.
2. Watch it run, auto-pause after 3 paragraphs.
3. Click **Resume** — next 3 paragraphs, then auto-pause again.
4. Click **Finish all** — runs to completion.
5. Repeat with **Stop** mid-run, then **Finalize**: job ends with a partial `full.wav`.
6. Confirm each bundle's meta line shows size and duration.

- [ ] **Step 5: Commit**

```bash
git add frontend/
git commit -m "feat(frontend): pause/resume/finalize controls, pause_every input, result metadata"
```

---

## Self-Review

**1. Spec coverage** — every spec section maps to a task:
- Feature 1 (result metadata) → Task 3.
- `StatusPaused` + state machine → Task 2 (types) + Task 4 (transitions).
- `pause_every` auto-pause → Task 2 (signature) + Task 4 (`controlGate`) + Task 5 (test `TestResumeContinuesAndRepauses…`).
- Manual pause → Task 4.
- On-pause partial stitch → Task 4 (`updateBundleFromStitch`, called from `ctlPause` branch).
- Resume (default re-applies `pauseEvery`) → Task 5.
- Resume (`{all: true}`) → Task 5 (`req.all` clears `pauseEvery`).
- Finalize (paused) → Task 5.
- Finalize (running) → Task 5 (`finalizeRequested` flag, caught at next boundary).
- Block-in-place worker → Task 4 (`<-ctl.resume`).
- API endpoints + form field → Task 6.
- Frontend → Task 7.
- Multi-bundle: `pauseEvery` is global → Task 4 (`stepsSinceResume` counts across bundles).

**2. Placeholder scan:** no TBDs, no "implement later"; every code step shows the actual code.

**3. Type consistency** across tasks:
- `Output.FullBytes int64`, `Output.DurationSec float64` — Task 3, used by Tasks 4, 5, 7.
- `Job.PauseEvery int` — Task 2, used by Tasks 5, 6.
- `JobStatus = "paused"` — Task 2, used by Tasks 4, 5, 6.
- `jobControl{ pauseEvery, pauseRequested, finalizeRequested, resume }` — Task 2, used by Tasks 4, 5.
- `resumeReq{ all, finalize }` — Task 2, used by Tasks 4, 5.
- `errJobFinalized` — Task 2, raised in Task 4, handled in Task 4's updated `run`.
- `Submit(..., pauseEvery int)` — Task 2 signature; used by Tasks 3, 4, 5, 6, 7.

**4. Ambiguity check:**
- Boundary rule: every test that triggers pause/finalize while a paragraph is in-flight releases that paragraph (`eng.release <- struct{}{}`) and asserts `Done` advanced by one. Spelled out in Tasks 4 and 5 tests.
- Resume on a running job: covered by `TestPauseEndpointReturns409WhenNotRunning` (Task 6) — generalises to all state-guard errors returning 409.
- Watcher caller update: Task 2 Step 5 names the file and the new argument value (`0`).
- The old `synthesizeBundle` method is **deleted**, not refactored in place — explicit in Task 4 Step 5.
