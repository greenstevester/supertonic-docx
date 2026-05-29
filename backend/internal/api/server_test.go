package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// buildMultipart builds a multipart/form-data body with the given fields and
// a single file upload. Returns body and Content-Type header.
func buildMultipart(t *testing.T, fields map[string]string, fileField, fileName string, fileBody []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := w.CreateFormFile(fileField, fileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(fileBody); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

// testDocxBytes returns a small valid docx as bytes (no on-disk fixture needed
// for HTTP tests).
func testDocxBytes(t *testing.T, paragraphs []string) []byte {
	t.Helper()
	p := writeTestDocx(t, paragraphs)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	j, ok := store.Get(resp.JobID)
	if !ok || j.PauseEvery != 2 {
		t.Errorf("PauseEvery = %d, want 2", j.PauseEvery)
	}

	// Wait for the worker to finish so t.TempDir cleanup doesn't race with
	// in-flight file writes.
	mustReachStatus(t, store, resp.JobID, StatusPaused)
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

func TestPauseResumeRoundTrip(t *testing.T) {
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
	if w.Code != http.StatusOK {
		t.Fatalf("pause status = %d", w.Code)
	}

	eng.release <- struct{}{} // boundary
	mustReachStatus(t, store, job.ID, StatusPaused)

	// Resume with {all:true} via HTTP.
	req = httptest.NewRequest("POST", "/api/jobs/"+job.ID+"/resume",
		bytes.NewBufferString(`{"all":true}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resume status = %d", w.Code)
	}

	releaseN(eng, 2)
	mustReachStatus(t, store, job.ID, StatusDone)
}

func TestFinalizeEndpointReturns409WhenDone(t *testing.T) {
	dir := t.TempDir()
	eng := newFakeEngine()
	store := NewJobStore(dir, eng, 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	docxPath := writeTestDocx(t, []string{"p1"})
	job, _ := store.Submit(docxPath, "t.docx", []string{"M1"}, []string{"en"}, 0)
	mustReachStatus(t, store, job.ID, StatusDone)

	req := httptest.NewRequest("POST", "/api/jobs/"+job.ID+"/finalize", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

// Symmetric to TestPauseEndpointReturns409WhenNotRunning — Resume on a job
// that is currently running (the worker is in-flight on a paragraph) must be
// rejected with 409, not silently swallowed.
func TestResumeEndpointReturns409WhenRunning(t *testing.T) {
	dir := t.TempDir()
	eng := newBlockingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	docxPath := writeTestDocx(t, []string{"p1", "p2"})
	job, _ := store.Submit(docxPath, "t.docx", []string{"M1"}, []string{"en"}, 0)
	<-eng.calls // worker now in-flight on p1, job status = running

	req := httptest.NewRequest("POST", "/api/jobs/"+job.ID+"/resume",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}

	// Drain so t.TempDir cleanup doesn't race with worker writes.
	eng.release <- struct{}{}
	releaseN(eng, 1)
	mustReachStatus(t, store, job.ID, StatusDone)
}

// All three control endpoints must distinguish "unknown id" (404) from
// state-guard violations (409). Without the errJobNotFound sentinel they all
// previously funnelled to 409, which is wrong per the spec.
func TestControlEndpointsReturn404OnUnknownJob(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir, newFakeEngine(), 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	for _, path := range []string{
		"/api/jobs/does-not-exist/pause",
		"/api/jobs/does-not-exist/resume",
		"/api/jobs/does-not-exist/finalize",
	} {
		req := httptest.NewRequest("POST", path, nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, w.Code)
		}
	}
}

// A second Pause while the first is still pending (worker hasn't reached the
// boundary yet) must be rejected — silently overwriting the flag would lose
// the second caller's intent on resume reset (the original race the
// "pause already pending" guard closes).
func TestPauseTwiceRejectsSecond(t *testing.T) {
	dir := t.TempDir()
	eng := newBlockingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	docxPath := writeTestDocx(t, []string{"p1", "p2"})

	job, _ := store.Submit(docxPath, "t.docx", []string{"M1"}, []string{"en"}, 0)
	<-eng.calls // worker in-flight on p1, status running

	if err := store.Pause(job.ID); err != nil {
		t.Fatalf("first Pause: %v", err)
	}
	if err := store.Pause(job.ID); err == nil ||
		!strings.Contains(err.Error(), "pending") {
		t.Errorf("second Pause: err = %v, want \"pause already pending\"", err)
	}

	// Boundary rule: let p1 complete; worker pauses at the boundary.
	eng.release <- struct{}{}
	mustReachStatus(t, store, job.ID, StatusPaused)
	// Finalize so t.TempDir cleanup is clean.
	if err := store.Finalize(job.ID); err != nil {
		t.Fatalf("cleanup Finalize: %v", err)
	}
	mustReachStatus(t, store, job.ID, StatusDone)
}
