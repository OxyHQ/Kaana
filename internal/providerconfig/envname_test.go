package providerconfig_test

import (
	"testing"

	"github.com/OxyHQ/Pensara/internal/providerconfig"
)

// The rename migration for environment variables.
//
// The image and the task definition deploy through different pipelines, so
// whichever moves first would leave a process reading names nothing sets —
// `PENSARA_PROVIDERS` unset means no provider is declared, which is a refusal to
// boot. The fallback removes the ordering constraint entirely.
//
// These tests are what makes it removable. When oxy-infra sets the new names,
// deleting the fallback turns TestTheLegacySpellingIsAnswered red, which is the
// signal that the deletion is a real change rather than a no-op.
func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

func TestTheCurrentSpellingIsAnswered(t *testing.T) {
	got := providerconfig.EnvName(env(map[string]string{"PENSARA_PROVIDERS": "groq"}), "PENSARA_PROVIDERS")
	if got != "groq" {
		t.Fatalf("EnvName = %q, want %q", got, "groq")
	}
}

func TestTheLegacySpellingIsAnswered(t *testing.T) {
	got := providerconfig.EnvName(env(map[string]string{"RELAY_PROVIDERS": "groq"}), "PENSARA_PROVIDERS")
	if got != "groq" {
		t.Fatalf("a deployment still setting RELAY_PROVIDERS got %q, want %q", got, "groq")
	}
}

// The current name wins, so the two can never disagree in a way that surprises
// anyone: a half-migrated deployment setting both reads the one it just set.
func TestTheCurrentSpellingWinsOverTheLegacyOne(t *testing.T) {
	got := providerconfig.EnvName(env(map[string]string{
		"PENSARA_PROVIDERS": "groq",
		"RELAY_PROVIDERS":   "cerebras",
	}), "PENSARA_PROVIDERS")
	if got != "groq" {
		t.Fatalf("EnvName = %q, want the current spelling %q", got, "groq")
	}
}

// Negative controls. Without these, an EnvName that answered everything — or one
// that stripped any prefix at all — would pass every assertion above.
func TestEnvNameRefusesWhatNobodySet(t *testing.T) {
	if got := providerconfig.EnvName(env(nil), "PENSARA_PROVIDERS"); got != "" {
		t.Errorf("an unset variable returned %q", got)
	}
	// A name that was never renamed has no legacy spelling to fall back to, and
	// must not acquire one.
	if got := providerconfig.EnvName(env(map[string]string{"RELAY_REGION": "usw2"}), "AWS_REGION"); got != "" {
		t.Errorf("a name outside the rename fell back to %q", got)
	}
}

// The prefix a provider slug reads its configuration from moved with the rest.
func TestEnvironmentPrefixUsesTheCurrentName(t *testing.T) {
	if got := providerconfig.EnvironmentPrefix("openrouter"); got != "PENSARA_PROVIDER_OPENROUTER" {
		t.Fatalf("EnvironmentPrefix = %q", got)
	}
	if got := providerconfig.EnvironmentPrefix("x-ai"); got != "PENSARA_PROVIDER_X_AI" {
		t.Fatalf("EnvironmentPrefix folded the separator wrongly: %q", got)
	}
}
