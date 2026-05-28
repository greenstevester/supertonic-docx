package api

// Engine is the minimal TTS surface JobStore depends on. The engine layer
// (internal/tts) already retries transient degenerate audio internally, so
// callers see either a valid 16-bit PCM mono WAV or a fatal error.
type Engine interface {
	Synthesize(text, voice, lang string) ([]byte, error)
	Voices() []string
	Languages() []string
	HasVoice(voice string) bool
	HasLang(lang string) bool
}
