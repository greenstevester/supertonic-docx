package tts

import "github.com/yourorg/supertonic-docx/internal/tts/supertonic_native"

// supportedLanguages returns the language codes the model accepts, taken
// directly from the vendored engine so there is a single source of truth.
func supportedLanguages() []string {
	out := make([]string, len(supertonic_native.AvailableLangs))
	copy(out, supertonic_native.AvailableLangs)
	return out
}

// LanguageName maps ISO codes to English display names for the UI. Codes with
// no entry here (e.g. the model's "na") fall back to the code itself at the
// API layer; the model doesn't care what we label them.
var LanguageName = map[string]string{
	"ar": "Arabic", "bg": "Bulgarian", "cs": "Czech", "da": "Danish",
	"de": "German", "el": "Greek", "en": "English", "es": "Spanish",
	"et": "Estonian", "fi": "Finnish", "fr": "French", "hi": "Hindi",
	"hr": "Croatian", "hu": "Hungarian", "id": "Indonesian", "it": "Italian",
	"ja": "Japanese", "ko": "Korean", "lt": "Lithuanian", "lv": "Latvian",
	"nl": "Dutch", "pl": "Polish", "pt": "Portuguese", "ro": "Romanian",
	"ru": "Russian", "sk": "Slovak", "sl": "Slovenian", "sv": "Swedish",
	"tr": "Turkish", "uk": "Ukrainian", "vi": "Vietnamese",
}
