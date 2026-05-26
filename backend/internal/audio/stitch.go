// Package audio handles concatenation of per-paragraph WAVs into a single
// "full.wav" per voice/language combination.
//
// Why custom code instead of shelling to ffmpeg?
//
// All of our input WAVs come from the same Supertonic engine: same sample
// rate, same bit depth, mono. That makes concatenation a header rewrite +
// data slice — no resampling, no format negotiation. Doing it in-process
// avoids ffmpeg as a runtime dependency and is materially faster for the
// 50-500 paragraph documents this tool targets.
//
// If we later support mixed-format inputs (e.g. user-provided intro music),
// swap this for an ffmpeg shell-out — but keep the interface.
package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// wavHeader is the 44-byte canonical PCM WAV header we read and write.
type wavHeader struct {
	SampleRate    uint32
	NumChannels   uint16
	BitsPerSample uint16
	DataSize      uint32
	DataOffset    int64 // byte offset where PCM samples begin
}

// Concatenate reads each path, validates that all files share the same
// audio format, and writes outPath as a single WAV with silenceMs of
// silence inserted between every pair of inputs.
func Concatenate(paths []string, outPath string, silenceMs int) error {
	if len(paths) == 0 {
		return fmt.Errorf("no inputs to concatenate")
	}

	// Read all headers first so we fail fast on a format mismatch, before
	// writing anything to outPath.
	headers := make([]wavHeader, len(paths))
	for i, p := range paths {
		h, err := readHeader(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		headers[i] = h
	}
	ref := headers[0]
	for i, h := range headers[1:] {
		if h.SampleRate != ref.SampleRate ||
			h.NumChannels != ref.NumChannels ||
			h.BitsPerSample != ref.BitsPerSample {
			return fmt.Errorf("format mismatch at %s (file %d)", paths[i+1], i+1)
		}
	}

	// Total data size = sum of input data + (N-1) gaps of silence.
	silenceBytes := silenceSamples(silenceMs, ref) * int(ref.BitsPerSample/8) * int(ref.NumChannels)
	totalData := uint32(silenceBytes) * uint32(len(paths)-1)
	for _, h := range headers {
		totalData += h.DataSize
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if err := writeHeader(out, ref, totalData); err != nil {
		return err
	}

	silence := make([]byte, silenceBytes) // zeros == silence in signed PCM

	for i, p := range paths {
		if i > 0 && silenceBytes > 0 {
			if _, err := out.Write(silence); err != nil {
				return err
			}
		}
		if err := copyPCM(out, p, headers[i]); err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}
	}
	return nil
}

func silenceSamples(ms int, h wavHeader) int {
	if ms <= 0 {
		return 0
	}
	return int(h.SampleRate) * ms / 1000
}

func readHeader(path string) (wavHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return wavHeader{}, err
	}
	defer f.Close()

	var buf [44]byte
	if _, err := io.ReadFull(f, buf[:]); err != nil {
		return wavHeader{}, fmt.Errorf("short header: %w", err)
	}
	if string(buf[0:4]) != "RIFF" || string(buf[8:12]) != "WAVE" {
		return wavHeader{}, fmt.Errorf("not a RIFF/WAVE file")
	}
	// We assume a canonical 44-byte header (fmt chunk size 16, no extra chunks
	// before data). Supertonic's output and our encoder both satisfy this; if
	// we ever ingest third-party WAVs we'd need a real chunk walker.
	if string(buf[12:16]) != "fmt " {
		return wavHeader{}, fmt.Errorf("missing fmt chunk")
	}
	if string(buf[36:40]) != "data" {
		return wavHeader{}, fmt.Errorf("non-canonical WAV layout (data chunk not at offset 36)")
	}

	return wavHeader{
		NumChannels:   binary.LittleEndian.Uint16(buf[22:24]),
		SampleRate:    binary.LittleEndian.Uint32(buf[24:28]),
		BitsPerSample: binary.LittleEndian.Uint16(buf[34:36]),
		DataSize:      binary.LittleEndian.Uint32(buf[40:44]),
		DataOffset:    44,
	}, nil
}

func writeHeader(w io.Writer, ref wavHeader, dataSize uint32) error {
	byteRate := ref.SampleRate * uint32(ref.NumChannels) * uint32(ref.BitsPerSample) / 8
	blockAlign := uint16(ref.NumChannels) * ref.BitsPerSample / 8

	var b [44]byte
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], 36+dataSize)
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	binary.LittleEndian.PutUint32(b[16:20], 16)
	binary.LittleEndian.PutUint16(b[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:24], ref.NumChannels)
	binary.LittleEndian.PutUint32(b[24:28], ref.SampleRate)
	binary.LittleEndian.PutUint32(b[28:32], byteRate)
	binary.LittleEndian.PutUint16(b[32:34], blockAlign)
	binary.LittleEndian.PutUint16(b[34:36], ref.BitsPerSample)
	copy(b[36:40], "data")
	binary.LittleEndian.PutUint32(b[40:44], dataSize)

	_, err := w.Write(b[:])
	return err
}

func copyPCM(w io.Writer, path string, h wavHeader) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(h.DataOffset, io.SeekStart); err != nil {
		return err
	}
	_, err = io.CopyN(w, f, int64(h.DataSize))
	return err
}

// Duration returns the playback length of a WAV in seconds. Used for
// manifest.json.
func Duration(path string) (float64, error) {
	h, err := readHeader(path)
	if err != nil {
		return 0, err
	}
	bytesPerSample := uint32(h.BitsPerSample/8) * uint32(h.NumChannels)
	if bytesPerSample == 0 {
		return 0, fmt.Errorf("invalid bytes/sample")
	}
	frames := h.DataSize / bytesPerSample
	return float64(frames) / float64(h.SampleRate), nil
}
