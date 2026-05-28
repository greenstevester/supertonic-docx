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
