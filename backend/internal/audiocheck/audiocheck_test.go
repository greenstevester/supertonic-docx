package audiocheck

import (
	"math"
	"testing"
)

func sine(n int, amp float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*440*float64(i)/24000))
	}
	return out
}

func TestGuard(t *testing.T) {
	tests := []struct {
		name    string
		samples []float32
		wantErr bool
	}{
		{"normal speech-like signal", sine(24000, 0.3), false},
		{"empty", nil, true},
		{"silent", make([]float32, 24000), true},
		{"near-silent below floor", sine(24000, 1e-6), true},
		{"all clipped", func() []float32 {
			s := make([]float32, 24000)
			for i := range s {
				s[i] = 1.0
			}
			return s
		}(), true},
		{"contains NaN", func() []float32 { s := sine(24000, 0.3); s[5] = float32(math.NaN()); return s }(), true},
		{"contains Inf", func() []float32 { s := sine(24000, 0.3); s[5] = float32(math.Inf(1)); return s }(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Guard(tt.samples, 24000)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Guard() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckReportsMetrics(t *testing.T) {
	r := Check(sine(24000, 0.3), 24000, Options{ExpectedSampleRate: 24000})
	if !r.OK {
		t.Fatalf("expected OK, got failures: %v", r.Failures)
	}
	if r.RMS <= 0 || r.Duration <= 0 {
		t.Fatalf("expected positive RMS/Duration, got rms=%v dur=%v", r.RMS, r.Duration)
	}
}

func TestCheckFlagsSampleRateAndDuration(t *testing.T) {
	r := Check(sine(24000, 0.3), 24000, Options{
		ExpectedSampleRate: 48000, // mismatch
		CharCount:          1000,  // 1s of audio for 1000 chars => below MinSecPerChar
		MinSecPerChar:      0.03,
		MaxSecPerChar:      0.30,
	})
	if r.OK {
		t.Fatal("expected failures for sample-rate mismatch + too-short duration")
	}
}

func TestReadWAVMono16RoundTrip(t *testing.T) {
	// Build a canonical 16-bit PCM mono WAV in memory, then decode it.
	samples := []float32{0, 0.5, -0.5, 1.0, -1.0}
	raw := writeTestWAV(samples, 24000)
	got, sr, err := ReadWAVMono16Bytes(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sr != 24000 {
		t.Fatalf("sample rate = %d, want 24000", sr)
	}
	if len(got) != len(samples) {
		t.Fatalf("len = %d, want %d", len(got), len(samples))
	}
	for i := range samples {
		if math.Abs(float64(got[i]-samples[i])) > 1.0/32767.0+1e-6 {
			t.Errorf("sample %d = %v, want ~%v", i, got[i], samples[i])
		}
	}
}

func writeTestWAV(samples []float32, sr int) []byte {
	clamp := func(s float32) int16 {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		return int16(math.Round(float64(s) * 32767))
	}
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		v := uint16(clamp(s))
		data[i*2] = byte(v)
		data[i*2+1] = byte(v >> 8)
	}
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	put32 := func(off int, v uint32) {
		hdr[off] = byte(v)
		hdr[off+1] = byte(v >> 8)
		hdr[off+2] = byte(v >> 16)
		hdr[off+3] = byte(v >> 24)
	}
	put16 := func(off int, v uint16) { hdr[off] = byte(v); hdr[off+1] = byte(v >> 8) }
	put32(4, uint32(36+len(data)))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	put32(16, 16)
	put16(20, 1)
	put16(22, 1)
	put32(24, uint32(sr))
	put32(28, uint32(sr*2))
	put16(32, 2)
	put16(34, 16)
	copy(hdr[36:40], "data")
	put32(40, uint32(len(data)))
	return append(hdr, data...)
}
