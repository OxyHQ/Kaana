package publisher

import (
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// openAINamespaces are the slugs that list OpenAI's own model ids, where the id
// alone names the request family a model's documented API requires. All three
// are bound to OpenAI's origin. A gateway that happens to carry an OpenAI model
// (OpenRouter's `openai/gpt-audio`) speaks its own namespace and is not
// classified here; its adapter refuses `audioOutput` in Translate instead.
var openAINamespaces = map[contract.ProviderSlug]bool{"openai": true, "openai-audio": true, "openai-realtime": true}

// family is what one OpenAI model id requires: a request family (and whether
// that family's output is SPOKEN), or a realtime session kind. Exactly one of
// format and session is set.
type family struct {
	format  contract.APIFormat
	spoken  bool
	session contract.RealtimeSessionKind
}

// openAIFamily classifies one of OpenAI's own model ids by what its
// documentation says it executes. The second return is false when nothing the
// contract names can express the model at all: GPT-Live is a separate session
// protocol (/v1/live/sessions), refused under every slug.
//
// Realtime ids name their session kind: gpt-realtime-translate translates,
// gpt-live-transcribe and gpt-realtime-whisper transcribe, and every other
// Realtime model holds conversations
// (https://developers.openai.com/api/docs/guides/realtime-translation,
// .../realtime-transcription). Whether a slug's adapter opens that kind is
// providerconfig.RealtimeSessionKinds' answer, not this function's.
//
// The audio chat models (`gpt-audio*`, `gpt-4o-audio-preview*`) are Chat
// Completions that answer aloud: chat_completions, spoken. That second bit is
// what keeps them off the text chat adapter, which executes chat_completions
// too but cannot produce audio, and keeps a text model off the audio adapter,
// whose chat_completions path produces nothing else.
//
// It is conservative in one direction only: an id it does not recognise is
// classified as text chat, which the chat adapter will still refuse to attach
// unless the attribution table names it. Attribution stays the allow-list; this
// is the check that a reviewed id is attached to an adapter that can execute it.
func openAIFamily(upstreamModelID string) (family, bool) {
	id := strings.ToLower(upstreamModelID)
	switch {
	case strings.HasPrefix(id, "gpt-live-transcribe"), strings.Contains(id, "realtime") && (strings.Contains(id, "whisper") || strings.Contains(id, "transcribe")):
		return family{session: contract.RealtimeTranscription}, true
	case strings.Contains(id, "realtime") && strings.Contains(id, "translate"):
		return family{session: contract.RealtimeTranslation}, true
	case strings.Contains(id, "realtime"):
		return family{session: contract.RealtimeConversation}, true
	case strings.HasPrefix(id, "gpt-live"):
		return family{}, false
	case strings.Contains(id, "moderation"):
		return family{}, false
	case strings.Contains(id, "transcribe"), strings.HasPrefix(id, "whisper"):
		return family{format: contract.APIFormatAudioTranscriptions}, true
	case strings.Contains(id, "tts"):
		return family{format: contract.APIFormatAudioSpeech}, true
	case strings.Contains(id, "audio"):
		return family{format: contract.APIFormatChatCompletions, spoken: true}, true
	case strings.HasPrefix(id, "dall-e"), strings.HasPrefix(id, "gpt-image"), strings.HasPrefix(id, "sora"):
		return family{format: contract.APIFormatImagesGenerations}, true
	case strings.Contains(id, "embedding"):
		return family{format: contract.APIFormatEmbeddings}, true
	}
	return family{format: contract.APIFormatChatCompletions}, true
}

// executable reports whether the provider's adapter can execute what a model
// requires: its request family, or its realtime session kind. Outside
// OpenAI's own namespace the model list says nothing about the family, so
// attribution alone decides, as before.
func executable(target Provider, upstreamModelID string) bool {
	if !openAINamespaces[target.Slug] {
		return true
	}
	required, expressible := openAIFamily(upstreamModelID)
	if !expressible {
		return false
	}
	protocol := target.Protocol
	if protocol == "" {
		protocol = providerconfig.Known[target.Slug].Protocol
	}
	if required.session != "" {
		for _, kind := range providerconfig.RealtimeSessionKinds(target.Slug, protocol) {
			if kind == required.session {
				return true
			}
		}
		return false
	}
	if required.format == contract.APIFormatChatCompletions && required.spoken != providerconfig.SpokenChatCompletions(protocol) {
		return false
	}
	for _, format := range providerconfig.ExecutableAPIFormats(target.Slug, protocol) {
		if format == required.format {
			return true
		}
	}
	return false
}
