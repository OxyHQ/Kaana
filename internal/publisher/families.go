package publisher

import (
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// executable reports whether the provider's adapter can execute what a model
// requires: its request family (and, for chat_completions, whether it answers
// aloud), or its realtime session kind. The classification is per slug and
// model (providerconfig.ClassifyModel); where the model list says nothing
// about the family, attribution alone decides, as before.
func executable(target Provider, upstreamModelID string) bool {
	required, classified, expressible := providerconfig.ClassifyModel(target.Slug, upstreamModelID)
	if !classified {
		return true
	}
	if !expressible {
		return false
	}
	protocol := target.Protocol
	if protocol == "" {
		protocol = providerconfig.Known[target.Slug].Protocol
	}
	if required.Session != "" {
		for _, kind := range providerconfig.RealtimeSessionKinds(target.Slug, protocol) {
			if kind == required.Session {
				return true
			}
		}
		return false
	}
	if required.Format == contract.APIFormatChatCompletions {
		// A model that answers aloud needs an adapter that can speak; a text
		// model needs one that writes. The audio adapter only speaks, the text
		// adapters only write, and OpenRouter's does both.
		outputs := providerconfig.ChatOutputs(target.Slug, protocol)
		if required.Spoken && !outputs.Spoken || !required.Spoken && !outputs.Text {
			return false
		}
	}
	for _, format := range providerconfig.ExecutableAPIFormats(target.Slug, protocol) {
		if format == required.Format {
			return true
		}
	}
	return false
}
