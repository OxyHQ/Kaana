package providerconfig

import "testing"

// A client names its own tag; the provider is sent its spelling of the
// language, or none for the model to detect.
func TestNormalizeLanguage(t *testing.T) {
	xai := []string{"en", "es-ES", "es-MX", "pt-BR", "pt-PT", "ar-EG", "ar-SA"}
	defaults := map[string]string{"ar": "ar-SA", "es": "es-MX", "pt": "pt-BR"}
	for _, c := range []struct {
		tag, want string
		ok        bool
		languages []string
	}{
		{"es-ES", "es-ES", true, xai},           // exact
		{"ES-es", "es-ES", true, xai},           // case-insensitively
		{"es-AR", "es-MX", true, xai},           // the provider's default variant
		{"es", "es-MX", true, xai},              // a bare language, the same
		{"en-GB", "en", true, xai},              // the language alone
		{"pt-AO", "pt-BR", true, xai},           // default over the first listed
		{"ar-MA", "ar-SA", true, xai},           // default over the first listed
		{"gl", "", false, xai},                  // nothing of it: detected
		{"es-AR", "es", true, whisperLanguages}, // Whisper's ISO 639-1
		{"zh-Hant-TW", "zh", true, whisperLanguages},
		{"tlh", "", false, whisperLanguages},
	} {
		got, ok := NormalizeLanguage(c.tag, c.languages, defaults)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeLanguage(%q) = %q, %v; want %q, %v", c.tag, got, ok, c.want, c.ok)
		}
	}
	// Without a default, the first variant of the language in order.
	if got, _ := NormalizeLanguage("fr-CA", []string{"fr-FR", "fr-BE"}, nil); got != "fr-FR" {
		t.Errorf("fr-CA without a default = %q", got)
	}
}
