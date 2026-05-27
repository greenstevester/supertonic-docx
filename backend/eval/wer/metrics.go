package main

import (
	"regexp"
	"strings"
)

var nonAlnum = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)
var spaces = regexp.MustCompile(`\s+`)

// normalize lowercases, strips punctuation/symbols, and collapses whitespace.
// Unicode-aware so it works for de/ja as well as en.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = nonAlnum.ReplaceAllString(s, "")
	s = spaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func levenshtein(a, b []string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// WER = word edit distance / reference word count.
func WER(ref, hyp string) float64 {
	r := strings.Fields(normalize(ref))
	h := strings.Fields(normalize(hyp))
	if len(r) == 0 {
		if len(h) == 0 {
			return 0
		}
		return 1
	}
	return float64(levenshtein(r, h)) / float64(len(r))
}

// CER = character edit distance / reference char count (spaces removed).
func CER(ref, hyp string) float64 {
	r := strings.Split(strings.ReplaceAll(normalize(ref), " ", ""), "")
	h := strings.Split(strings.ReplaceAll(normalize(hyp), " ", ""), "")
	if len(r) == 0 {
		if len(h) == 0 {
			return 0
		}
		return 1
	}
	return float64(levenshtein(r, h)) / float64(len(r))
}
