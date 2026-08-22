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
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// The wire protocols this repository speaks. Provider slugs are not a closed
// list, but protocols are: a build can only construct an adapter it contains,
// so a slug declaring an unknown protocol is refused rather than guessed at.
const (
	ProtocolOpenAICompatible  = "openai_compatible"
	ProtocolAnthropicMessages = "anthropic_messages"
)

// Known is the protocol and address of the providers this build carries
// built in, so a slug from this set needs nothing but its key.
//
// The roots are the providers' published ones. No live call has been made from
// this repository to any of them by a human reading a documentation page; the
// publisher command is the first thing here that calls one at all, and it does
// so only with an operator-supplied credential.
var Known = map[contract.ProviderSlug]Endpoint{
	"openai":     {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.openai.com/v1"},
	"anthropic":  {Protocol: ProtocolAnthropicMessages, BaseURL: "https://api.anthropic.com/v1"},
	"openrouter": {Protocol: ProtocolOpenAICompatible, BaseURL: "https://openrouter.ai/api/v1"},
	"cerebras":   {Protocol: ProtocolOpenAICompatible, BaseURL: "https://api.cerebras.ai/v1"},
}

// Endpoint is where a provider is reached and what it speaks there.
type Endpoint struct {
	Protocol string
	BaseURL  string
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
// winner's address and credentials.
//
// This name is also the SSM parameter leaf and the GitHub secret name. They are
// one string on purpose: the deploy sync derives the parameter path from the
// secret name, so a design where they differ breaks the sync silently.
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

// EnvName is the one place that knows this service used to be called Relay.
//
// Every variable it reads is spelled `KAANA_*`; a deployment still setting the
// `RELAY_*` spelling is answered from that instead. The two cannot disagree in
// a way that surprises anyone, because the current name always wins and the
// legacy one is only consulted when the current is unset.
//
// This is a MIGRATION with an end, not a compatibility layer. The variables are
// set by one place — the task definition in oxy-infra — so once that names the
// new ones, this function collapses to its first line and the tests that prove
// the fallback works turn red, which is how the deletion announces that it did
// something.
//
// Renaming without it is not merely risky, it is unorderable: the image and the
// task definition deploy through different pipelines, so whichever moves first
// leaves a process reading names nothing sets. For RELAY_PROVIDERS that is
// `no provider is declared` and a refusal to boot.
func EnvName(getenv func(string) string, name string) string {
	if value := getenv(name); value != "" {
		return value
	}
	legacy, renamed := strings.CutPrefix(name, "KAANA_")
	if !renamed {
		return ""
	}
	return getenv("RELAY_" + legacy)
}
