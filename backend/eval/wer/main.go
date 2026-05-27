// Command eval-wer is the Tier-2 intelligibility round-trip: synthesize each
// corpus entry with the real Engine, transcribe with a user-supplied ASR
// wrapper, and score WER/CER against per-language thresholds.
//
// Usage:
//
//	SUPERTONIC_ASSETS=../assets go run ./eval/wer \
//	  -corpus eval/wer/corpus.json -thresholds eval/wer/thresholds.json \
//	  -asr eval/asr.sh -out /tmp/wer-out
//
// The ASR wrapper is exec'd as:  <asr> <wav-path> <lang>  and must print the
// transcript to stdout. See eval/asr.sh.example.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yourorg/supertonic-docx/internal/tts"
)

type corpusEntry struct {
	ID    string `json:"id"`
	Lang  string `json:"lang"`
	Voice string `json:"voice"`
	Text  string `json:"text"`
}

type threshold struct {
	Metric string  `json:"metric"` // "wer" | "cer"
	Max    float64 `json:"max"`
}

func main() {
	assets := flag.String("assets", os.Getenv("SUPERTONIC_ASSETS"), "model assets dir")
	corpusPath := flag.String("corpus", "eval/wer/corpus.json", "corpus JSON")
	thrPath := flag.String("thresholds", "eval/wer/thresholds.json", "thresholds JSON")
	asr := flag.String("asr", "eval/asr.sh", "ASR wrapper: <asr> <wav> <lang> -> transcript on stdout")
	outDir := flag.String("out", "/tmp/wer-out", "where to write generated WAVs")
	flag.Parse()
	if *assets == "" {
		fmt.Fprintln(os.Stderr, "missing -assets / SUPERTONIC_ASSETS")
		os.Exit(2)
	}

	corpus := mustLoadCorpus(*corpusPath)
	thresholds := mustLoadThresholds(*thrPath)

	eng, err := tts.NewEngine(*assets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewEngine: %v\n", err)
		os.Exit(1)
	}
	defer eng.Close()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var failures int
	for _, e := range corpus {
		wav, err := eng.Synthesize(e.Text, e.Voice, e.Lang)
		if err != nil {
			failures++
			fmt.Printf("FAIL  %s: synth: %v\n", e.ID, err)
			continue
		}
		wavPath := filepath.Join(*outDir, e.ID+".wav")
		if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		transcript, err := transcribe(*asr, wavPath, e.Lang)
		if err != nil {
			failures++
			fmt.Printf("FAIL  %s: asr: %v\n", e.ID, err)
			continue
		}

		thr, ok := thresholds[e.Lang]
		if !ok {
			failures++
			fmt.Printf("FAIL  %s: no threshold for lang %q (add it to thresholds.json)\n", e.ID, e.Lang)
			continue
		}
		var score float64
		switch thr.Metric {
		case "cer":
			score = CER(e.Text, transcript)
		default:
			score = WER(e.Text, transcript)
		}
		status := "ok  "
		if score > thr.Max {
			status = "FAIL"
			failures++
		}
		fmt.Printf("%s  %s  %s=%.3f (max %.3f)  hyp=%q\n", status, e.ID, thr.Metric, score, thr.Max, transcript)
	}

	fmt.Printf("\n%d entries, %d failed\n", len(corpus), failures)
	if failures > 0 {
		os.Exit(1)
	}
}

func transcribe(asr, wav, lang string) (string, error) {
	out, err := exec.Command(asr, wav, lang).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func mustLoadCorpus(path string) []corpusEntry {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var c []corpusEntry
	if err := json.Unmarshal(b, &c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return c
}

func mustLoadThresholds(path string) map[string]threshold {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var t map[string]threshold
	if err := json.Unmarshal(b, &t); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return t
}
