package main

import (
	"math"
	"testing"
)

func TestNormalize(t *testing.T) {
	got := normalize("Hello, World!  It's  $5.")
	want := "hello world its 5"
	if got != want {
		t.Fatalf("normalize = %q, want %q", got, want)
	}
}

func TestWER(t *testing.T) {
	tests := []struct {
		ref, hyp string
		want     float64
	}{
		{"the cat sat", "the cat sat", 0.0},
		{"the cat sat", "the dog sat", 1.0 / 3.0},               // 1 sub / 3 ref words
		{"the cat sat on the mat", "the cat on mat", 2.0 / 6.0}, // 2 deletions
		{"", "", 0.0},
	}
	for _, tt := range tests {
		if got := WER(tt.ref, tt.hyp); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("WER(%q,%q) = %v, want %v", tt.ref, tt.hyp, got, tt.want)
		}
	}
}

func TestCER(t *testing.T) {
	if got := CER("abc", "abc"); got != 0 {
		t.Errorf("CER identical = %v, want 0", got)
	}
	if got := CER("abc", "abd"); math.Abs(got-1.0/3.0) > 1e-9 {
		t.Errorf("CER 1 sub = %v, want 1/3", got)
	}
}
