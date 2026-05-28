// Package tts wraps Supertonic 3 ONNX inference behind a small, stable
// interface the rest of the service depends on.
//
// The tensor orchestration lives in the vendored upstream package
// internal/tts/supertonic_native (see its VENDORED.md). This file is the
// seam: everything Supertonic-specific is reachable only through Engine, so
// the api/ and audio/ packages never import ONNX types.
//
// ONNX Runtime sessions are not goroutine-safe, so Synthesize serialises all
// inference behind e.mu. The engine is loaded once at startup and shared
// across every HTTP request and watcher event.
package tts

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/yourorg/supertonic-docx/internal/audiocheck"
	"github.com/yourorg/supertonic-docx/internal/tts/supertonic_native"
)

// Hardcoded inference parameters (design decision: not user-configurable).
const (
	totalStep       = 8             // denoising steps (upstream default)
	speed           = float32(1.05) // speech rate (upstream default)
	chunkSilenceSec = float32(0.3)  // gap between sub-chunks of long text (upstream example default)

	// maxSynthAttempts bounds how many times a single paragraph is synthesized
	// when the result comes back degenerate (silent/clipped/non-finite). The
	// flow-matching sampler draws a fresh noise latent on every Call (seeded
	// per call from time), so an unlucky draw that collapses to silence is
	// transient — re-sampling almost always recovers. Degenerate draws are
	// rare, so the extra cost is paid only on the rare failure, and it keeps a
	// single bad paragraph from aborting a multi-thousand-paragraph job.
	maxSynthAttempts = 4
)

// Engine is a loaded Supertonic model + voice/language catalogue. Safe for
// concurrent use; inference is serialised internally because ONNX Runtime
// sessions are not goroutine-safe.
type Engine struct {
	assetsDir string

	mu     sync.Mutex // serialises ONNX session calls and the style cache
	model  *supertonic_native.TextToSpeech
	sr     int                                 // sample rate from tts.json (AE.SampleRate)
	styles map[string]*supertonic_native.Style // voice name -> loaded style (lazy, cached)

	voices   []string          // sorted, deduplicated
	voiceMap map[string]string // voice name -> voice_styles/<name>.json (empty when using fallback list)
	langs    []string          // from vendored AvailableLangs
}

// NewEngine loads the model assets from assetsDir.
//
// Expected layout (Supertonic 3 HF repo):
//
//	assetsDir/
//	  onnx/
//	    tts.json
//	    unicode_indexer.json
//	    text_encoder.onnx  duration_predictor.onnx
//	    vector_estimator.onnx  vocoder.onnx
//	  voice_styles/
//	    M1.json  F1.json  ...
func NewEngine(assetsDir string) (*Engine, error) {
	if _, err := os.Stat(assetsDir); err != nil {
		return nil, fmt.Errorf("assets dir %s: %w", assetsDir, err)
	}

	// Initialise ONNX Runtime once. The vendored helper resolves the library
	// from ONNXRUNTIME_LIB_PATH (fallback /usr/local/lib/libonnxruntime.so).
	if err := supertonic_native.InitializeONNXRuntime(); err != nil {
		return nil, fmt.Errorf("init ONNX Runtime (set ONNXRUNTIME_LIB_PATH if libonnxruntime is not on the default path): %w", err)
	}

	onnxDir := filepath.Join(assetsDir, "onnx")
	cfg, err := supertonic_native.LoadCfgs(onnxDir)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", filepath.Join(onnxDir, "tts.json"), err)
	}

	// LoadTextToSpeech opens the 4 ONNX sessions and loads unicode_indexer.json
	// internally; useGPU is always false (CPU-only, per design).
	model, err := supertonic_native.LoadTextToSpeech(onnxDir, false, cfg)
	if err != nil {
		return nil, fmt.Errorf("open ONNX sessions in %s: %w", onnxDir, err)
	}

	e := &Engine{
		assetsDir: assetsDir,
		model:     model,
		sr:        model.SampleRate,
		styles:    map[string]*supertonic_native.Style{},
		voiceMap:  map[string]string{},
		langs:     supportedLanguages(),
	}
	if err := e.discoverVoices(); err != nil {
		model.Destroy()
		return nil, err
	}
	return e, nil
}

// Close releases the ONNX sessions, cached style tensors, and the ORT env.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.styles {
		st.Destroy()
	}
	e.styles = map[string]*supertonic_native.Style{}
	if e.model != nil {
		e.model.Destroy()
		e.model = nil
	}
	ort.DestroyEnvironment()
	return nil
}

// Voices returns the available voice presets, sorted.
func (e *Engine) Voices() []string {
	out := make([]string, len(e.voices))
	copy(out, e.voices)
	return out
}

// Languages returns the supported language codes.
func (e *Engine) Languages() []string {
	out := make([]string, len(e.langs))
	copy(out, e.langs)
	return out
}

// HasVoice / HasLang validate requests before a (potentially long) job starts.
func (e *Engine) HasVoice(v string) bool { _, ok := e.voiceMap[v]; return ok }
func (e *Engine) HasLang(l string) bool {
	for _, c := range e.langs {
		if c == l {
			return true
		}
	}
	return false
}

// Synthesize generates a 16-bit PCM mono WAV for one paragraph in one
// voice/lang. Returns the raw .wav bytes ready to write to disk.
func (e *Engine) Synthesize(text, voice, lang string) ([]byte, error) {
	if !e.HasVoice(voice) {
		return nil, fmt.Errorf("unknown voice %q", voice)
	}
	if !e.HasLang(lang) {
		return nil, fmt.Errorf("unknown lang %q", lang)
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("empty text")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	samples, sr, err := e.synthesizeOne(text, voice, lang)
	if err != nil {
		return nil, err
	}
	return encodeWAV(samples, sr), nil
}

// synthesizeOne is the only function that touches Supertonic directly.
// Caller must hold e.mu.
func (e *Engine) synthesizeOne(text, voice, lang string) ([]float32, int, error) {
	style, err := e.styleFor(voice)
	if err != nil {
		return nil, 0, err
	}
	// Tier-0 runtime guard: a silent/degenerate result is a transient sampling
	// artifact (see maxSynthAttempts), so re-sample before giving up. Only a
	// paragraph that is degenerate on every attempt becomes a loud error.
	samples, err := synthesizeWithGuard(func() ([]float32, error) {
		s, _, callErr := e.model.Call(text, lang, style, totalStep, speed, chunkSilenceSec)
		if callErr != nil {
			return nil, fmt.Errorf("inference: %w", callErr)
		}
		return s, nil
	}, e.sr, maxSynthAttempts)
	if err != nil {
		return nil, 0, fmt.Errorf("degenerate audio after %d attempts (voice=%s lang=%s text=%q): %w", maxSynthAttempts, voice, lang, truncate(text, 60), err)
	}
	return samples, e.sr, nil
}

// synthesizeWithGuard runs call and validates its output with audiocheck.Guard,
// retrying up to attempts times when the audio is degenerate (silent, fully
// clipped, or non-finite). Re-sampling recovers because the flow-matching
// sampler re-seeds on every call. A genuine inference error (call returns a
// non-nil error) is fatal and is never retried. If every attempt is degenerate,
// the last guard failure is returned.
func synthesizeWithGuard(call func() ([]float32, error), sampleRate, attempts int) ([]float32, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastGuardErr error
	for i := 0; i < attempts; i++ {
		samples, err := call()
		if err != nil {
			return nil, err
		}
		if guardErr := audiocheck.Guard(samples, sampleRate); guardErr != nil {
			lastGuardErr = guardErr
			continue
		}
		return samples, nil
	}
	return nil, lastGuardErr
}

// styleFor lazily loads and caches a voice's style tensors. Caller holds e.mu.
func (e *Engine) styleFor(voice string) (*supertonic_native.Style, error) {
	if st, ok := e.styles[voice]; ok {
		return st, nil
	}
	path := e.voiceMap[voice]
	if path == "" {
		return nil, fmt.Errorf("voice %q has no style file (are assets fetched into voice_styles/?)", voice)
	}
	st, err := supertonic_native.LoadVoiceStyle([]string{path}, false)
	if err != nil {
		return nil, fmt.Errorf("load voice style %s: %w", path, err)
	}
	e.styles[voice] = st
	return st, nil
}

// discoverVoices reads assetsDir/voice_styles/*.json; voice name = filename
// without ".json". Falls back to a hardcoded preset list only when the dir is
// missing, so the UI is usable before assets are fetched.
func (e *Engine) discoverVoices() error {
	dir := filepath.Join(e.assetsDir, "voice_styles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		e.useFallbackVoices()
		return nil
	}
	seen := map[string]bool{}
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		voice := strings.TrimSuffix(ent.Name(), ".json")
		if seen[voice] {
			continue
		}
		seen[voice] = true
		e.voices = append(e.voices, voice)
		e.voiceMap[voice] = filepath.Join(dir, ent.Name())
	}
	sort.Strings(e.voices)
	if len(e.voices) == 0 {
		e.useFallbackVoices()
	}
	return nil
}

func (e *Engine) useFallbackVoices() {
	for _, v := range fallbackVoices() {
		e.voices = append(e.voices, v)
		e.voiceMap[v] = "" // no style file; styleFor will reject synthesis
	}
	sort.Strings(e.voices)
}

func fallbackVoices() []string {
	return []string{"M1", "M2", "M3", "M4", "M5", "F1", "F2", "F3", "F4", "F5"}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// encodeWAV writes a 16-bit PCM mono WAV in memory. Float samples are clamped
// to [-1, 1] then scaled to int16. The sample rate is supplied by the caller
// (from tts.json), not hardcoded.
func encodeWAV(samples []float32, sampleRate int) []byte {
	const (
		bitsPerSample = 16
		numChannels   = 1
	)
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataSize := len(samples) * 2 // int16

	buf := make([]byte, 0, 44+dataSize)
	w := func(v ...byte) { buf = append(buf, v...) }
	wU32 := func(v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		w(b[:]...)
	}
	wU16 := func(v uint16) {
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], v)
		w(b[:]...)
	}

	w([]byte("RIFF")...)
	wU32(uint32(36 + dataSize))
	w([]byte("WAVE")...)

	w([]byte("fmt ")...)
	wU32(16)
	wU16(1)
	wU16(uint16(numChannels))
	wU32(uint32(sampleRate))
	wU32(uint32(byteRate))
	wU16(uint16(blockAlign))
	wU16(uint16(bitsPerSample))

	w([]byte("data")...)
	wU32(uint32(dataSize))
	for _, s := range samples {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		i := int16(math.Round(float64(s) * 32767))
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(i))
		buf = append(buf, b[:]...)
	}
	return buf
}
