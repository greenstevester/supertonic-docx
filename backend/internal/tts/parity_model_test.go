//go:build model_evals

// Tier 1 parity. Run only with real assets + ONNX Runtime present:
//
//	SUPERTONIC_ASSETS=../../../assets go test -tags model_evals ./internal/tts -run Parity -v
package tts

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/yourorg/supertonic-docx/internal/tts/supertonic_native"
)

const parityText = "This morning, I took a walk in the park." // upstream example default

func TestParityWrapperVsDirect(t *testing.T) {
	assets := os.Getenv("SUPERTONIC_ASSETS")
	if assets == "" {
		t.Skip("set SUPERTONIC_ASSETS to the model dir to run parity")
	}
	const (
		voice = "M1"
		lang  = "en"
		seed  = int64(1234)
	)

	// Engine side: NewEngine initialises ORT globally and opens sessions.
	e, err := NewEngine(assets)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer e.Close()
	e.model.SeedFunc = func() int64 { return seed } // pin (white-box)

	got, sr, err := e.synthesizeOne(parityText, voice, lang)
	if err != nil {
		t.Fatalf("synthesizeOne: %v", err)
	}

	// Direct side: ORT env is already initialised by NewEngine, so just load a
	// second model + style and Call with the same params + seed.
	onnxDir := filepath.Join(assets, "onnx")
	cfg, err := supertonic_native.LoadCfgs(onnxDir)
	if err != nil {
		t.Fatalf("LoadCfgs: %v", err)
	}
	model, err := supertonic_native.LoadTextToSpeech(onnxDir, false, cfg)
	if err != nil {
		t.Fatalf("LoadTextToSpeech: %v", err)
	}
	defer model.Destroy()
	model.SeedFunc = func() int64 { return seed }

	style, err := supertonic_native.LoadVoiceStyle([]string{filepath.Join(assets, "voice_styles", voice+".json")}, false)
	if err != nil {
		t.Fatalf("LoadVoiceStyle: %v", err)
	}
	defer func() {
		style.TtlTensor.Destroy()
		style.DpTensor.Destroy()
	}()

	want, _, err := model.Call(parityText, lang, style, totalStep, speed, chunkSilenceSec)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if sr != model.SampleRate {
		t.Fatalf("sample rate mismatch: engine %d vs direct %d", sr, model.SampleRate)
	}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: engine %d vs direct %d", len(got), len(want))
	}
	var sumSq float64
	for i := range got {
		d := float64(got[i] - want[i])
		sumSq += d * d
	}
	rmsDiff := math.Sqrt(sumSq / float64(len(got)))
	if rmsDiff > 1e-3 {
		t.Fatalf("RMS diff %.3g exceeds 1e-3 — wrapper diverges from direct call", rmsDiff)
	}
	t.Logf("parity OK: %d samples, rms diff %.3g", len(got), rmsDiff)
}
