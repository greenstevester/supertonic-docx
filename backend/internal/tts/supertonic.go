// Package tts wraps Supertonic 3 ONNX inference behind a small, stable
// interface that the rest of the service can depend on.
//
// Why a wrapper?
//
// The upstream supertonic Go example (github.com/supertone-inc/supertonic/go)
// is shipped as an `example_onnx.go` program, not a reusable library. Its
// internals — tokenizer setup, voice-style loading, ONNX session lifecycle —
// are convenient to call from main() but awkward to embed in a service.
//
// This file is the seam: everything Supertonic-specific lives here. If the
// upstream API changes (or moves to a real library), only this file needs
// updating. The api/ and audio/ packages talk to Engine, not to ONNX.
//
// Wiring the real model in
// ------------------------
// The actual ONNX session calls are stubbed with TODOs marked SUPERTONIC.
// To wire them up:
//
//  1. Vendor the upstream files into internal/tts/supertonic_native/
//     (helper.go, tokenizer helpers, and any phonemiser tables).
//  2. Replace the stub in synthesizeOne with calls into that package.
//  3. The shape of the ONNX inputs/outputs is documented at
//     https://github.com/supertone-inc/supertonic/blob/main/go/example_onnx.go
//     and is stable between Supertonic 2 and 3 — only the asset filenames
//     differ. The model expects (text-tokens, voice-style-embedding, lang-id)
//     and returns a float32 PCM waveform at 44.1 kHz.
//
// Until that's done, Synthesize returns a short silent WAV so the end-to-end
// pipeline (docx → API → file links) can be developed and tested without
// having the model present.
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
)

// Engine is a loaded Supertonic model + voice/language catalogue.
// Safe for concurrent use; synthesizeOne is serialised internally because
// ONNX Runtime sessions are not goroutine-safe by default.
type Engine struct {
	assetsDir string

	mu       sync.Mutex        // serialises ONNX session calls
	voices   []string          // sorted, deduplicated
	voiceMap map[string]string // voice name -> path to style file
	langs    []string          // canonical 31 codes
}

// NewEngine loads the model assets from assetsDir.
//
// Expected layout (matches the supertonic-3 HF repo):
//
//	assetsDir/
//	  model.onnx              (or whatever the v3 asset bundle names it)
//	  voices/
//	    M1.bin
//	    F1.bin
//	    ...
//	  tokenizer/...
func NewEngine(assetsDir string) (*Engine, error) {
	if _, err := os.Stat(assetsDir); err != nil {
		return nil, fmt.Errorf("assets dir %s: %w", assetsDir, err)
	}

	e := &Engine{
		assetsDir: assetsDir,
		voiceMap:  map[string]string{},
		langs:     supportedLanguages(),
	}

	if err := e.discoverVoices(); err != nil {
		return nil, err
	}

	// SUPERTONIC: load model.onnx into an ONNX Runtime session here.
	// e.session, err = onnxruntime.NewSession(filepath.Join(assetsDir, "model.onnx"), ...)

	return e, nil
}

// Close releases any held resources (ONNX session, etc.).
func (e *Engine) Close() error {
	// SUPERTONIC: e.session.Destroy()
	return nil
}

// Voices returns the available voice presets, sorted.
func (e *Engine) Voices() []string {
	out := make([]string, len(e.voices))
	copy(out, e.voices)
	return out
}

// Languages returns the supported language codes, sorted.
func (e *Engine) Languages() []string {
	out := make([]string, len(e.langs))
	copy(out, e.langs)
	return out
}

// HasVoice / HasLang are used by the API layer to validate requests
// before kicking off a (potentially long) job.
func (e *Engine) HasVoice(v string) bool { _, ok := e.voiceMap[v]; return ok }
func (e *Engine) HasLang(l string) bool {
	for _, c := range e.langs {
		if c == l {
			return true
		}
	}
	return false
}

// Synthesize generates a WAV (16-bit PCM) for one paragraph in one voice/lang.
// Returns the raw .wav bytes ready to write to disk.
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
// Everything above it is generic; everything below is, too. Swap this body
// out when wiring the real ONNX session.
func (e *Engine) synthesizeOne(text, voice, lang string) ([]float32, int, error) {
	// SUPERTONIC: real implementation:
	//   1. tokens := tokenizer.Encode(text, lang)
	//   2. style  := loadStyle(e.voiceMap[voice])
	//   3. langID := langCodeToID[lang]
	//   4. outs, err := e.session.Run(map[string]any{
	//          "tokens": tokens, "style": style, "lang_id": langID,
	//      })
	//   5. return outs["waveform"].([]float32), 44100, nil

	// Stub: 250 ms of silence at 44.1 kHz so the pipeline runs end-to-end.
	const sr = 44100
	durSec := 0.25 + float64(len(text))*0.06 // rough fake "duration"
	n := int(durSec * float64(sr))
	if n > sr*30 {
		n = sr * 30
	}
	return make([]float32, n), sr, nil
}

// discoverVoices reads assetsDir/voices/ and registers anything that looks
// like a voice file (M1.bin, F1.bin, ...). This avoids hard-coding the
// preset list — when Supertonic ships new voices, dropping them into the
// assets dir is enough.
func (e *Engine) discoverVoices() error {
	voicesDir := filepath.Join(e.assetsDir, "voices")
	entries, err := os.ReadDir(voicesDir)
	if err != nil {
		// Not fatal — let the engine come up with a fallback voice list
		// so the UI is usable even before assets are fully downloaded.
		// The API layer's job validation will catch the mismatch.
		for _, v := range fallbackVoices() {
			e.voices = append(e.voices, v)
			e.voiceMap[v] = "" // no path; synthesizeOne stub doesn't need it
		}
		sort.Strings(e.voices)
		return nil
	}

	seen := map[string]bool{}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		ext := filepath.Ext(name)
		if ext != ".bin" && ext != ".npy" && ext != ".onnx" {
			continue
		}
		voice := strings.TrimSuffix(name, ext)
		if seen[voice] {
			continue
		}
		seen[voice] = true
		e.voices = append(e.voices, voice)
		e.voiceMap[voice] = filepath.Join(voicesDir, name)
	}
	sort.Strings(e.voices)

	if len(e.voices) == 0 {
		// Same fallback path as above.
		for _, v := range fallbackVoices() {
			e.voices = append(e.voices, v)
			e.voiceMap[v] = ""
		}
		sort.Strings(e.voices)
	}
	return nil
}

func fallbackVoices() []string {
	return []string{"M1", "M2", "M3", "M4", "M5", "F1", "F2", "F3", "F4", "F5"}
}

// encodeWAV writes a 16-bit PCM mono WAV file in memory.
// Float samples are clamped to [-1, 1] then scaled to int16.
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

	// RIFF header
	w([]byte("RIFF")...)
	wU32(uint32(36 + dataSize))
	w([]byte("WAVE")...)

	// fmt chunk
	w([]byte("fmt ")...)
	wU32(16) // PCM chunk size
	wU16(1)  // PCM format
	wU16(uint16(numChannels))
	wU32(uint32(sampleRate))
	wU32(uint32(byteRate))
	wU16(uint16(blockAlign))
	wU16(uint16(bitsPerSample))

	// data chunk
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
