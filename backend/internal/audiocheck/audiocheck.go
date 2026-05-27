// Package audiocheck validates synthesized PCM audio. It backs two callers:
// the runtime Guard inside the TTS engine (turns silent-success into a loud
// job error) and the standalone Tier-0 outbox scanner. Checks are cheap and
// format-agnostic; they assume float32 samples in [-1, 1].
package audiocheck

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"
)

// Default thresholds. Tuned conservatively: real speech sits well above the
// RMS floor and well below the clip ratio; anything that trips these is
// degenerate (silence, DC, full-scale noise, NaN/Inf).
const (
	defaultRMSFloor    = 1e-4 // mean-square energy floor
	defaultMaxClipRate = 0.02 // fraction of |s| >= clipLevel tolerated
	clipLevel          = 0.999
)

// Options configures Check. Zero values disable the optional duration band.
type Options struct {
	ExpectedSampleRate int     // if > 0, mismatch is a failure
	RMSFloor           float64 // 0 => defaultRMSFloor
	MaxClipRate        float64 // 0 => defaultMaxClipRate
	CharCount          int     // input length; enables duration band when > 0
	MinSecPerChar      float64 // duration band lower bound (per char)
	MaxSecPerChar      float64 // duration band upper bound (per char)
}

// Result is the full report from Check.
type Result struct {
	OK       bool
	RMS      float64
	ClipRate float64
	Duration float64
	Failures []string
}

// Check runs all applicable checks and returns a report.
func Check(samples []float32, sampleRate int, opt Options) Result {
	rmsFloor := opt.RMSFloor
	if rmsFloor == 0 {
		rmsFloor = defaultRMSFloor
	}
	maxClip := opt.MaxClipRate
	if maxClip == 0 {
		maxClip = defaultMaxClipRate
	}

	r := Result{OK: true}
	if len(samples) == 0 {
		r.OK = false
		r.Failures = append(r.Failures, "empty audio")
		return r
	}

	var sumSq float64
	var clipped int
	for _, s := range samples {
		f := float64(s)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			r.OK = false
			r.Failures = append(r.Failures, "non-finite sample (NaN/Inf)")
			return r
		}
		sumSq += f * f
		if math.Abs(f) >= clipLevel {
			clipped++
		}
	}
	r.RMS = math.Sqrt(sumSq / float64(len(samples)))
	r.ClipRate = float64(clipped) / float64(len(samples))
	if sampleRate > 0 {
		r.Duration = float64(len(samples)) / float64(sampleRate)
	}

	if r.RMS < rmsFloor {
		r.OK = false
		r.Failures = append(r.Failures, fmt.Sprintf("silent: rms %.3g < floor %.3g", r.RMS, rmsFloor))
	}
	if r.ClipRate > maxClip {
		r.OK = false
		r.Failures = append(r.Failures, fmt.Sprintf("clipped: %.1f%% of samples at full scale", r.ClipRate*100))
	}
	if opt.ExpectedSampleRate > 0 && sampleRate != opt.ExpectedSampleRate {
		r.OK = false
		r.Failures = append(r.Failures, fmt.Sprintf("sample rate %d != expected %d", sampleRate, opt.ExpectedSampleRate))
	}
	if opt.CharCount > 0 && opt.MaxSecPerChar > 0 {
		lo := float64(opt.CharCount) * opt.MinSecPerChar
		hi := float64(opt.CharCount) * opt.MaxSecPerChar
		if r.Duration < lo || r.Duration > hi {
			r.OK = false
			r.Failures = append(r.Failures, fmt.Sprintf("duration %.2fs outside band [%.2f, %.2f]s for %d chars", r.Duration, lo, hi, opt.CharCount))
		}
	}
	return r
}

// Guard is the runtime check used inside the engine: cheap, always-correct
// checks only (non-empty, finite, non-silent, not fully clipped). Sample-rate
// and duration-band checks are the standalone tool's job.
func Guard(samples []float32, sampleRate int) error {
	r := Check(samples, sampleRate, Options{})
	if !r.OK {
		return fmt.Errorf("%s", strings.Join(r.Failures, "; "))
	}
	return nil
}

// ReadWAVMono16 reads a canonical 16-bit PCM mono WAV (the layout our encoder
// and Supertonic both produce) into float32 samples in [-1, 1].
func ReadWAVMono16(path string) ([]float32, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return ReadWAVMono16Bytes(raw)
}

// ReadWAVMono16Bytes is ReadWAVMono16 for an in-memory buffer.
func ReadWAVMono16Bytes(raw []byte) ([]float32, int, error) {
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a RIFF/WAVE buffer")
	}
	if string(raw[36:40]) != "data" {
		return nil, 0, fmt.Errorf("non-canonical WAV layout (data chunk not at offset 36)")
	}
	sampleRate := int(binary.LittleEndian.Uint32(raw[24:28]))
	bits := binary.LittleEndian.Uint16(raw[34:36])
	if bits != 16 {
		return nil, 0, fmt.Errorf("expected 16-bit PCM, got %d", bits)
	}
	dataSize := int(binary.LittleEndian.Uint32(raw[40:44]))
	if 44+dataSize > len(raw) {
		dataSize = len(raw) - 44
	}
	n := dataSize / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(raw[44+i*2 : 46+i*2]))
		out[i] = float32(v) / 32768.0
	}
	return out, sampleRate, nil
}
