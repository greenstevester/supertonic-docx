package tts

import (
	"encoding/binary"
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
