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
func (f *fakeEngine) Voices() []string    { return f.voices }
func (f *fakeEngine) Languages() []string { return f.langs }
func (f *fakeEngine) HasVoice(v string) bool {
	for _, x := range f.voices {
		if x == v {
			return true
		}
	}
	return false
}
func (f *fakeEngine) HasLang(l string) bool {
	for _, x := range f.langs {
		if x == l {
			return true
		}
	}
	return false
}

// makeFakeWAV builds a canonical mono 16-bit PCM WAV with n constant-amplitude
// samples — well above any RMS floor, well below clip.
func makeFakeWAV(n, sr int) []byte {
	const amp int16 = 8000
	const bits, ch = 16, 1
	dataSize := n * 2
	buf := bytes.NewBuffer(make([]byte, 0, 44+dataSize))
	mustWrite := func(v any) {
		if err := binary.Write(buf, binary.LittleEndian, v); err != nil {
			panic(err) // bytes.Buffer Write never fails
		}
	}
	buf.WriteString("RIFF")
	mustWrite(uint32(36 + dataSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	mustWrite(uint32(16))
	mustWrite(uint16(1))
	mustWrite(uint16(ch))
	mustWrite(uint32(sr))
	mustWrite(uint32(sr * ch * bits / 8))
	mustWrite(uint16(ch * bits / 8))
	mustWrite(uint16(bits))
	buf.WriteString("data")
	mustWrite(uint32(dataSize))
	for i := 0; i < n; i++ {
		mustWrite(amp)
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
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
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
		if fn() {
			return true
		}
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
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

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

func TestPauseStitchesPartialFullWav(t *testing.T) {
	dir := t.TempDir()
	eng := newBlockingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	docxPath := writeTestDocx(t, []string{"p1", "p2", "p3", "p4", "p5"})

	job, err := store.Submit(docxPath, "test.docx", []string{"M1"}, []string{"en"}, 0)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Release 2 paragraphs.
	releaseN(eng, 2)

	// Worker is now blocked at paragraph 3's Synthesize call.
	<-eng.calls // observe the call to p3

	// Request pause while p3 is in-flight (blocked on release).
	if err := store.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

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

func TestResumeContinuesAndRepausesAfterPauseEvery(t *testing.T) {
	dir := t.TempDir()
	eng := newBlockingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	docxPath := writeTestDocx(t, []string{"p1", "p2", "p3", "p4", "p5"})

	job, err := store.Submit(docxPath, "test.docx", []string{"M1"}, []string{"en"}, 2)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// First 2 paragraphs synthesize, then auto-pause.
	releaseN(eng, 2)
	mustReachStatus(t, store, job.ID, StatusPaused)
	if j, _ := store.Get(job.ID); j.Progress.Done != 2 {
		t.Errorf("expected Done=2 at first pause, got %d", j.Progress.Done)
	}

	// Resume (default re-applies pause_every).
	if err := store.Resume(job.ID, false); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	releaseN(eng, 2)
	mustReachStatus(t, store, job.ID, StatusPaused)
	if j, _ := store.Get(job.ID); j.Progress.Done != 4 {
		t.Errorf("expected Done=4 at second pause, got %d", j.Progress.Done)
	}

	// Final resume with all=true — runs to completion.
	if err := store.Resume(job.ID, true); err != nil {
		t.Fatalf("Resume(all): %v", err)
	}
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

	if err := store.Finalize(job.ID); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
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

	if err := store.Finalize(job.ID); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
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
