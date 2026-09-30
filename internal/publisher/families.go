package publisher

import (
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// openAINamespaces are the slugs that list OpenAI's own model ids, where the id
// alone names the request family a model's documented API requires. Both are
// bound to OpenAI's origin. A gateway that happens to carry an OpenAI model
// (OpenRouter's `openai/gpt-audio`) speaks its own namespace and is not
// classified here.
var openAINamespaces = map[contract.ProviderSlug]bool{"openai": true, "openai-audio": true}

// openAIRequestFamily classifies one of OpenAI's own model ids by the request
// family its documentation says it executes. The second return is false when no
// contract family can express it at all: Realtime and Live are session
// protocols, and the audio chat models produce audio output Chat Completions
// has no request shape for. Those are refused under every slug.
//
// It is conservative in one direction only: an id it does not recognise is
// classified as chat, which the chat adapter will still refuse to attach unless
// the attribution table names it. Attribution stays the allow-list; this is the
// check that a reviewed id is attached to an adapter that can execute it.
func openAIRequestFamily(upstreamModelID string) (contract.APIFormat, bool) {
	id := strings.ToLower(upstreamModelID)
	switch {
	case strings.Contains(id, "realtime"), strings.HasPrefix(id, "gpt-live"):
		return "", false
	case strings.Contains(id, "audio"):
		return "", false
	case strings.Contains(id, "moderation"):
		return "", false
	case strings.Contains(id, "transcribe"), strings.HasPrefix(id, "whisper"):
		return contract.APIFormatAudioTranscriptions, true
	case strings.Contains(id, "tts"):
		return contract.APIFormatAudioSpeech, true
	case strings.HasPrefix(id, "dall-e"), strings.HasPrefix(id, "gpt-image"), strings.HasPrefix(id, "sora"):
		return contract.APIFormatImagesGenerations, true
	case strings.Contains(id, "embedding"):
		return contract.APIFormatEmbeddings, true
	}
	return contract.APIFormatChatCompletions, true
}

// executable reports whether the provider's adapter can execute the request
// family a model requires. Outside OpenAI's own namespace the model list says
// nothing about the family, so attribution alone decides, as before.
func executable(target Provider, upstreamModelID string) bool {
	if !openAINamespaces[target.Slug] {
		return true
	}
	family, expressible := openAIRequestFamily(upstreamModelID)
	if !expressible {
		return false
	}
	protocol := target.Protocol
	if protocol == "" {
		protocol = providerconfig.Known[target.Slug].Protocol
	}
	for _, format := range providerconfig.ExecutableAPIFormats(target.Slug, protocol) {
		if format == family {
			return true
		}
	}
	return false
}
