// Command eval-audiocheck scans an outbox job dir (or any dir tree of WAVs)
// and reports Tier-0 audio sanity per file: non-silent, finite, not clipped,
// sample rate == expected, duration within a per-char band.
//
// Usage:
//
//	go run ./eval/audiocheck -dir ../outbox/job-<id> -sr 24000
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yourorg/supertonic-docx/internal/audiocheck"
)

type sourceManifest struct {
	Paragraphs []struct {
		Index int    `json:"index"`
		Text  string `json:"text"`
	} `json:"paragraphs"`
}

func main() {
	dir := flag.String("dir", "", "job dir to scan (e.g. ../outbox/job-<id>)")
	sr := flag.Int("sr", 0, "expected sample rate (0 = skip the check)")
	minSPC := flag.Float64("min-sec-per-char", 0.015, "duration band lower bound per char")
	maxSPC := flag.Float64("max-sec-per-char", 0.40, "duration band upper bound per char")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "missing -dir")
		os.Exit(2)
	}

	// Map para index -> char count from source.json, if present.
	chars := map[int]int{}
	if b, err := os.ReadFile(filepath.Join(*dir, "source.json")); err == nil {
		var sm sourceManifest
		if json.Unmarshal(b, &sm) == nil {
			for _, p := range sm.Paragraphs {
				chars[p.Index] = len([]rune(p.Text))
			}
		}
	}

	var total, failed int
	err := filepath.WalkDir(*dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".wav") {
			return nil
		}
		total++
		samples, gotSR, err := audiocheck.ReadWAVMono16(path)
		if err != nil {
			failed++
			fmt.Printf("FAIL  %s: %v\n", path, err)
			return nil
		}
		opt := audiocheck.Options{ExpectedSampleRate: *sr}
		// Duration band only for per-paragraph files we have char counts for.
		if base := filepath.Base(path); strings.HasPrefix(base, "para_") {
			var idx int
			fmt.Sscanf(base, "para_%03d.wav", &idx)
			if c, ok := chars[idx]; ok && c > 0 {
				opt.CharCount = c
				opt.MinSecPerChar = *minSPC
				opt.MaxSecPerChar = *maxSPC
			}
		}
		r := audiocheck.Check(samples, gotSR, opt)
		if r.OK {
			fmt.Printf("ok    %s  rms=%.3g dur=%.2fs sr=%d\n", path, r.RMS, r.Duration, gotSR)
		} else {
			failed++
			fmt.Printf("FAIL  %s: %s\n", path, strings.Join(r.Failures, "; "))
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("\n%d files, %d failed\n", total, failed)
	if failed > 0 {
		os.Exit(1)
	}
}
