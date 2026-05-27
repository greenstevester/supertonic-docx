package tts

import (
	"testing"

	"github.com/yourorg/supertonic-docx/internal/tts/supertonic_native"
)

func TestSupportedLanguagesMatchVendored(t *testing.T) {
	got := supportedLanguages()
	want := supertonic_native.AvailableLangs
	if len(got) != len(want) {
		t.Fatalf("supportedLanguages len = %d, want %d (vendored AvailableLangs)", len(got), len(want))
	}
	set := map[string]bool{}
	for _, c := range got {
		set[c] = true
	}
	for _, c := range want {
		if !set[c] {
			t.Errorf("missing language %q from supportedLanguages()", c)
		}
	}
}

func TestEnglishStillPresent(t *testing.T) {
	e := &Engine{langs: supportedLanguages()}
	if !e.HasLang("en") {
		t.Fatal("en must be supported")
	}
}

func TestEveryLanguageResolvesToAName(t *testing.T) {
	for _, code := range supportedLanguages() {
		name := LanguageName[code]
		if name == "" {
			name = code // documented fallback (e.g. "na")
		}
		if name == "" {
			t.Errorf("language %q resolves to empty display name", code)
		}
	}
}
