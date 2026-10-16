package providerconfig

import (
	"slices"
	"strings"
)

// NormalizeLanguage is the tag to send a provider for the BCP 47 tag a client
// named, from the tags the provider takes (Kaana ADR 0003: a client names its
// language; which spelling a provider wants is Kaana's business):
//
//  1. the tag itself, compared case-insensitively;
//  2. its language alone (`es` for `es-AR`), when the provider takes that;
//  3. the provider's default variant of the language (`es-MX` for `es-AR` on
//     a provider that needs a region), from defaults;
//  4. any variant of the language the provider takes, the first in order.
//
// ok is false when the provider takes nothing of the language: the request
// is then sent without one, and a model that detects the language does.
func NormalizeLanguage(tag string, languages []string, defaults map[string]string) (string, bool) {
	if exact := slices.IndexFunc(languages, func(listed string) bool { return strings.EqualFold(listed, tag) }); exact >= 0 {
		return languages[exact], true
	}
	primary := strings.ToLower(strings.SplitN(tag, "-", 2)[0])
	if slices.Contains(languages, primary) {
		return primary, true
	}
	if variant, ok := defaults[primary]; ok && slices.Contains(languages, variant) {
		return variant, true
	}
	for _, listed := range languages {
		if strings.EqualFold(strings.SplitN(listed, "-", 2)[0], primary) {
			return listed, true
		}
	}
	return "", false
}

// whisperLanguages are the ISO 639-1 codes of the languages OpenAI's Whisper
// transcribes: the two-letter keys of LANGUAGES in its tokenizer
// (https://github.com/openai/whisper/blob/main/whisper/tokenizer.py, MIT), read
// 2026-10-16. A Whisper endpoint (Groq; OpenAI's `whisper-1`) takes exactly
// these as `language`; the three-letter `haw` and `yue` and the non-standard
// `jw` are left out, so a request in them is transcribed by detection.
var whisperLanguages = []string{
	"af", "am", "ar", "as", "az", "ba", "be", "bg", "bn", "bo", "br", "bs", "ca", "cs", "cy", "da", "de",
	"el", "en", "es", "et", "eu", "fa", "fi", "fo", "fr", "gl", "gu", "ha", "he", "hi", "hr", "ht", "hu",
	"hy", "id", "is", "it", "ja", "ka", "kk", "km", "kn", "ko", "la", "lb", "ln", "lo", "lt", "lv", "mg",
	"mi", "mk", "ml", "mn", "mr", "ms", "mt", "my", "ne", "nl", "nn", "no", "oc", "pa", "pl", "ps", "pt",
	"ro", "ru", "sa", "sd", "si", "sk", "sl", "sn", "so", "sq", "sr", "su", "sv", "sw", "ta", "te", "tg",
	"th", "tk", "tl", "tr", "tt", "uk", "ur", "uz", "vi", "yi", "yo", "zh",
}
