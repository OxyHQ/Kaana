// Package providerconfig holds the parts of a provider's configuration that
// BOTH the serving process and the inventory publisher have to agree on: the
// environment variable a slug reads, the address it is reached at, and the
// wire protocol it speaks.
//
// It exists because there are now two commands. `cmd/kaana` resolves a slug to
// an adapter, an address and a credential POOL; `cmd/kaana-publisher` resolves
// the same slug to an address and ONE credential so it can ask the provider
// which models it serves. The address and the variable name are the same fact
// in both, and a second copy of them would drift the day a base URL moves —
// silently, because each command would still be internally consistent.
//
// What is deliberately NOT here is everything only the serving process needs:
// the key pool and its rotation policy, the extra headers, the protocol's
// mapping onto an adapter this build actually contains. Those belong beside the
// adapters they configure, and hoisting them would make this package a second
// place to look for how a request is sent.
package providerconfig

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// The wire protocols this repository speaks. Provider slugs are not a closed
// list, but protocols are: a build can only construct an adapter it contains,
// so a slug declaring an unknown protocol is refused rather than guessed at.
const (
	ProtocolOpenAICompatible  = "openai_compatible"
	ProtocolAnthropicMessages = "anthropic_messages"

	DiscoveryOpenAIModels  = "openai_models"
	DiscoveryMistralModels = "mistral_models"
	DiscoverySiliconModels = "siliconflow_models"
	DiscoveryNotAvailable  = "not_available"
)

// Known is the protocol and address of the providers this build carries
// built in, so a slug from this set needs nothing but its key.
//
// The roots are the providers' published ones. No live call has been made from
// this repository to any of them by a human reading a documentation page; the
// publisher command is the first thing here that calls one at all, and it does
// so only with an operator-supplied credential.
var Known = map[contract.ProviderSlug]Endpoint{
	"openai":       {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.openai.com/v1", Discovery: DiscoveryOpenAIModels},
	"anthropic":    {Protocol: ProtocolAnthropicMessages, BaseURL: "https://api.anthropic.com/v1", Discovery: DiscoveryNotAvailable},
	"openrouter":   {Protocol: ProtocolOpenAICompatible, BaseURL: "https://openrouter.ai/api/v1", Discovery: DiscoveryOpenAIModels},
	"cerebras":     {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.cerebras.ai/v1", Discovery: DiscoveryOpenAIModels},
	"groq":         {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.groq.com/openai/v1", Discovery: DiscoveryOpenAIModels},
	"xai":          {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.x.ai/v1", Discovery: DiscoveryOpenAIModels},
	"mistral":      {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.mistral.ai/v1", Discovery: DiscoveryMistralModels},
	"deepseek":     {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.deepseek.com", Discovery: DiscoveryOpenAIModels},
	"sambanova":    {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.sambanova.ai/v1", Discovery: DiscoveryOpenAIModels},
	"siliconflow":  {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.siliconflow.cn/v1", Discovery: DiscoverySiliconModels},
	"ai21":         {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.ai21.com/studio/v1", Discovery: DiscoveryNotAvailable},
	"google":       {Protocol: ProtocolOpenAICompatible, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Discovery: DiscoveryNotAvailable},
	"together":     {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.together.ai/v1", Discovery: DiscoveryOpenAIModels},
	"cohere":       {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.cohere.ai/compatibility/v1", Discovery: DiscoveryNotAvailable},
	"fireworks":    {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.fireworks.ai/inference/v1", Discovery: DiscoveryNotAvailable},
	"hyperbolic":   {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.hyperbolic.xyz/v1", Discovery: DiscoveryNotAvailable},
	"digitalocean": {Protocol: ProtocolOpenAICompatible, BaseURL: "https://inference.do-ai.run/v1", Discovery: DiscoveryOpenAIModels},
	"nvidia":       {Protocol: ProtocolOpenAICompatible, BaseURL: "https://integrate.api.nvidia.com/v1", Discovery: DiscoveryNotAvailable},
	"modelscope":   {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api-inference.modelscope.cn/v1", Discovery: DiscoveryNotAvailable},
	"zai":          {Protocol: ProtocolOpenAICompatible, BaseURL: "https://open.bigmodel.cn/api/paas/v4", Discovery: DiscoveryNotAvailable},
}

// ValidateBaseURL limits provider credentials to a verified HTTPS origin.
func ValidateBaseURL(raw string) error {
	if contract.CredentialShaped(raw) {
		return fmt.Errorf("provider base URL must not contain credential-shaped data; provider credentials belong only in Kaana's encrypted database")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("provider base URL must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	return nil
}

// Endpoint is where a provider is reached and what it speaks there.
type Endpoint struct {
	Protocol string
	BaseURL  string
	// Discovery identifies the provider's documented account model-list
	// shape. Serving compatibility never implies discovery compatibility.
	Discovery string
}

// EnvironmentPrefix is the variable name a slug reads its configuration from.
//
// The transform is total and its output is always a legal environment variable
// name: a slug is `[a-z0-9._-]`, the two characters an environment variable
// cannot carry are folded to `_`, and the constant prefix means the result
// never begins with a digit. Nothing is unrepresentable, so nothing has to be
// rejected for being unspellable.
//
// What the folding DOES create is collisions — `open-router` and `open.router`
// are two slugs and one variable name — and every caller refuses those rather
// than resolving them, because the loser would silently be configured with the
// winner's adapter configuration.
//
// This prefix is only for non-secret adapter configuration. Provider keys are
// PostgreSQL rows and never acquire an environment name.
func EnvironmentPrefix(slug contract.ProviderSlug) string {
	replaced := strings.NewReplacer(".", "_", "-", "_").Replace(string(slug))
	return "KAANA_PROVIDER_" + strings.ToUpper(replaced)
}

// SplitList reads a comma-separated environment value, discarding the empty
// entries a trailing separator leaves behind.
func SplitList(value string) []string {
	items := make([]string, 0, 4)
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}
