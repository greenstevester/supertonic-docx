package api

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/gin-gonic/gin"
)

// voiceSamplePhrase is the neutral English sentence rendered for every voice
// preview. A pangram keeps the sample phonetically broad without being long.
const voiceSamplePhrase = "The quick brown fox jumps over the lazy dog."

// voiceSampleCacheDir is the subfolder of outbox where pre-rendered voice
// samples live. Samples are voice-stable (text + lang are fixed), so once
// written they can be served indefinitely.
const voiceSampleCacheDir = "_voice_samples"

// voiceSampleMutexes serialises rendering for a given voice so concurrent
// first-touches don't both kick off a synthesize. Per-voice keys; the engine
// itself serialises ONNX calls internally, but this avoids redundant work and
// avoids writing the same file twice.
var voiceSampleMutexes sync.Map // map[string]*sync.Mutex

func voiceSampleMutex(voice string) *sync.Mutex {
	m, _ := voiceSampleMutexes.LoadOrStore(voice, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// voiceSample returns a short pre-rendered preview WAV for the given voice.
// First request synthesises and caches to disk; subsequent requests serve
// straight from the cache. 404 on unknown voices, 500 on synthesis failure.
func (h *handlers) voiceSample(c *gin.Context) {
	voice := c.Param("voice")
	if !h.opts.Jobs.engine.HasVoice(voice) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown voice"})
		return
	}

	dir := filepath.Join(h.opts.OutboxDir, voiceSampleCacheDir)
	path := filepath.Join(dir, voice+".wav")

	if _, err := os.Stat(path); err == nil {
		h.servePreviewWAV(c, path)
		return
	}

	mu := voiceSampleMutex(voice)
	mu.Lock()
	defer mu.Unlock()

	// Double-check under the lock — another goroutine may have rendered it
	// while we were waiting.
	if _, err := os.Stat(path); err == nil {
		h.servePreviewWAV(c, path)
		return
	}

	wav, err := h.opts.Jobs.engine.Synthesize(voiceSamplePhrase, voice, "en")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := os.WriteFile(path, wav, 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.servePreviewWAV(c, path)
}

// servePreviewWAV streams the cached file with a long Cache-Control so the
// browser can avoid revisiting the endpoint for the lifetime of the page.
func (h *handlers) servePreviewWAV(c *gin.Context, path string) {
	c.Header("Content-Type", "audio/wav")
	c.Header("Cache-Control", "public, max-age=86400")
	c.Header("Content-Disposition", `inline; filename="preview.wav"`)
	c.File(path)
}
