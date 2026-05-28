package tts

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverVoicesFromVoiceStyles(t *testing.T) {
	dir := t.TempDir()
	vs := filepath.Join(dir, "voice_styles")
	if err := os.MkdirAll(vs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"M1.json", "F2.json", "notes.txt", "F2.json"} {
		if err := os.WriteFile(filepath.Join(vs, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := &Engine{assetsDir: dir, voiceMap: map[string]string{}}
	if err := e.discoverVoices(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"F2", "M1"}; !reflect.DeepEqual(e.Voices(), want) {
		t.Fatalf("voices = %v, want %v", e.Voices(), want)
	}
	if e.voiceMap["M1"] != filepath.Join(vs, "M1.json") {
		t.Fatalf("M1 path = %q", e.voiceMap["M1"])
	}
}

func TestDiscoverVoicesFallsBackWhenDirMissing(t *testing.T) {
	e := &Engine{assetsDir: t.TempDir(), voiceMap: map[string]string{}} // no voice_styles/
	if err := e.discoverVoices(); err != nil {
		t.Fatal(err)
	}
	if len(e.Voices()) != len(fallbackVoices()) {
		t.Fatalf("expected fallback voices, got %v", e.Voices())
	}
	if e.HasVoice("M1") == false {
		t.Fatal("fallback should include M1")
	}
}

func TestEncodeWAVRoundTripsConfiguredSampleRate(t *testing.T) {
	for _, sr := range []int{24000, 44100} {
		raw := encodeWAV([]float32{0, 0.5, -0.5, 1.5, -1.5}, sr) // last two clamp
		if got := int(binary.LittleEndian.Uint32(raw[24:28])); got != sr {
			t.Errorf("header sample rate = %d, want %d", got, sr)
		}
		dataSize := int(binary.LittleEndian.Uint32(raw[40:44]))
		if dataSize != 5*2 {
			t.Errorf("data size = %d, want 10", dataSize)
		}
		// clamp check: 1.5 -> +32767, -1.5 -> -32767
		last := int16(binary.LittleEndian.Uint16(raw[44+8 : 44+10]))
		if last != -32767 {
			t.Errorf("clamp: last sample = %d, want -32767", last)
		}
	}
}

// constSignal returns n samples at amp. amp=0 is silent (RMS 0, below the
// audiocheck floor -> degenerate); a non-zero amp well under full scale passes.
func constSignal(n int, amp float32) []float32 {
	s := make([]float32, n)
	for i := range s {
		s[i] = amp
	}
	return s
}

// Regression: a single paragraph that comes back silent on an unlucky sampler
// seed used to abort the whole job. synthesizeWithGuard must re-sample and
// recover, since a degenerate draw is transient (the sampler re-seeds per call).
func TestSynthesizeWithGuardRecoversFromTransientDegenerate(t *testing.T) {
	const sr = 44100
	good := constSignal(sr/10, 0.3) // 0.1s, RMS 0.3 >> floor
	silent := constSignal(sr/10, 0) // RMS 0 < floor -> degenerate

	calls := 0
	got, err := synthesizeWithGuard(func() ([]float32, error) {
		calls++
		if calls < 3 { // first two draws collapse to silence
			return silent, nil
		}
		return good, nil
	}, sr, maxSynthAttempts)
	if err != nil {
		t.Fatalf("expected recovery after re-sampling, got error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts (2 degenerate + 1 good), got %d", calls)
	}
	if !reflect.DeepEqual(got, good) {
		t.Fatal("expected the good (non-degenerate) samples to be returned")
	}
}

// The retry must stay bounded: persistently degenerate output is still a loud
// error after maxSynthAttempts, not an infinite loop or a silent pass.
func TestSynthesizeWithGuardFailsWhenEveryAttemptDegenerate(t *testing.T) {
	const sr = 44100
	silent := constSignal(100, 0)
	calls := 0
	_, err := synthesizeWithGuard(func() ([]float32, error) {
		calls++
		return silent, nil
	}, sr, maxSynthAttempts)
	if err == nil {
		t.Fatal("expected an error when every attempt is degenerate")
	}
	if calls != maxSynthAttempts {
		t.Fatalf("expected %d attempts, got %d", maxSynthAttempts, calls)
	}
}

// A genuine inference error is not a transient sampling artifact, so it must
// propagate immediately without burning retries (and without being masked).
func TestSynthesizeWithGuardDoesNotRetryInferenceError(t *testing.T) {
	boom := errors.New("inference boom")
	calls := 0
	_, err := synthesizeWithGuard(func() ([]float32, error) {
		calls++
		return nil, boom
	}, 44100, maxSynthAttempts)
	if calls != 1 {
		t.Fatalf("a genuine inference error must not be retried; got %d calls", calls)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected the inference error to propagate, got %v", err)
	}
}
