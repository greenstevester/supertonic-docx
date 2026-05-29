package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// countingFakeEngine wraps fakeEngine with an atomic call counter so tests can
// verify caching (one Synthesize per voice across N requests) and concurrency
// (one Synthesize even when N requests race).
type countingFakeEngine struct {
	*fakeEngine
	calls int64
}

func newCountingFakeEngine() *countingFakeEngine {
	return &countingFakeEngine{fakeEngine: newFakeEngine()}
}

func (c *countingFakeEngine) Synthesize(text, voice, lang string) ([]byte, error) {
	atomic.AddInt64(&c.calls, 1)
	return c.fakeEngine.Synthesize(text, voice, lang)
}

func TestVoiceSampleReturns404OnUnknownVoice(t *testing.T) {
	dir := t.TempDir()
	store := NewJobStore(dir, newFakeEngine(), 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	req := httptest.NewRequest("GET", "/api/voice-samples/Z9", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestVoiceSampleRendersAndCachesOnFirstRequest(t *testing.T) {
	dir := t.TempDir()
	eng := newCountingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	req := httptest.NewRequest("GET", "/api/voice-samples/M1", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav", ct)
	}
	if w.Body.Len() <= 44 {
		t.Errorf("body length = %d, want > 44 (WAV header)", w.Body.Len())
	}
	cached := filepath.Join(dir, voiceSampleCacheDir, "M1.wav")
	if _, err := os.Stat(cached); err != nil {
		t.Errorf("cache file not written at %s: %v", cached, err)
	}
	if got := atomic.LoadInt64(&eng.calls); got != 1 {
		t.Errorf("Synthesize called %d times on first request, want 1", got)
	}
}

func TestVoiceSampleSecondRequestUsesCache(t *testing.T) {
	dir := t.TempDir()
	eng := newCountingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/api/voice-samples/M1", nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, w.Code)
		}
	}
	if got := atomic.LoadInt64(&eng.calls); got != 1 {
		t.Errorf("Synthesize called %d times across 3 requests, want 1 (caching)", got)
	}
}

func TestVoiceSampleConcurrentFirstTouchesRenderOnce(t *testing.T) {
	dir := t.TempDir()
	eng := newCountingFakeEngine()
	store := NewJobStore(dir, eng, 0)
	srv := NewServer(ServerOpts{Jobs: store, OutboxDir: dir})

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/api/voice-samples/F1", nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("concurrent: status = %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&eng.calls); got != 1 {
		t.Errorf("Synthesize called %d times across %d concurrent requests, want 1", got, n)
	}
}
