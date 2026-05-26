package tts

// supportedLanguages returns the 31 language codes baked into Supertonic 3.
// Source: https://huggingface.co/Supertone/supertonic-3#supported-languages
//
// Kept in code (not a config file) so the binary is self-contained and the
// frontend can fetch this list via /api/catalogue without any extra mounts.
func supportedLanguages() []string {
	return []string{
		"ar", "bg", "cs", "da", "de", "el", "en", "es", "et", "fi",
		"fr", "hi", "hr", "hu", "id", "it", "ja", "ko", "lt", "lv",
		"nl", "pl", "pt", "ro", "ru", "sk", "sl", "sv", "tr", "uk",
		"vi",
	}
}

// LanguageName maps ISO codes to display names for the UI.
// English-only — the UI is for English-speaking operators; the model
// itself doesn't care what we call these.
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
