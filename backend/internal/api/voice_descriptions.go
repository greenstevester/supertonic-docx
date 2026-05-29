package api

// voiceDescriptions are short human-facing descriptions of each voice preset
// shown as a subtitle on the picker chip. Edit freely; entries missing from
// this map simply render without a subtitle (and Tier-0 fallback voices
// likewise have no description by default). Descriptions flow through the
// catalogue API at runtime — restart the server to pick up edits, no
// frontend rebuild required.
var voiceDescriptions = map[string]string{
	"F1": "Emily — 24, primary-school teacher from Connecticut.",
	"F2": "Clara — Santa Clara. History at Berkeley, has a cat, shops at Whole Foods.",
	"F3": "Mary-Anne — mid-40s ex-vocal-coach. Knits, volunteers at the local animal rescue. No passport. Loves Trivial Pursuit.",
	"F4": "Molly — San Francisco. Tech, independent, weekend BBQs and walks with her dog Boof.",
	"F5": "Abigail — Surrey, England. Works locally, university-educated, reads and visits old castles.",
	"M1": "Jason — 35, office worker, plaid, ADHD, fastidiously neat, eats every meal at the same time.",
	"M2": "Brendan — ex-footballer, runs daily, Atlanta, tech.",
	"M3": "Luke — Starbucks barista with acting ambitions.",
	"M4": "Marvin — 32, works at a Lego/Marvel-toys franchise five minutes from his parents' house, where he still lives.",
	"M5": "Malcolm — Sloane Square, London SW1. Retail, shops locally, rarely leaves a five-mile radius.",
}
