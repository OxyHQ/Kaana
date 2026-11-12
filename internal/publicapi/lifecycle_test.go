package publicapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func lifecycleCatalogue(deprecation string) *PublicCatalogue {
	return &PublicCatalogue{models: map[contract.ModelID]declaredModel{
		"acme/old": {fields: map[string]json.RawMessage{"deprecation": json.RawMessage(deprecation)}},
	}}
}

func targetOf(reference string) *modelTarget {
	ref := contract.ModelReference(reference)
	return &modelTarget{reference: &ref}
}

func TestADeprecatedModelAnnouncesItsSunsetAndSuccessor(t *testing.T) {
	catalogue := lifecycleCatalogue(`{"status":"deprecated","announcedAt":"2026-07-20T00:00:00.000Z","sunsetAt":"2027-01-20T00:00:00.000Z","replacementModelReference":"acme/new"}`)
	header := http.Header{}
	catalogue.announceLifecycle(header, targetOf("acme/old"))
	if got := header.Get(HeaderDeprecation); got != "@1784505600" {
		t.Errorf("Deprecation = %q", got)
	}
	if got := header.Get(HeaderSunset); got != "Wed, 20 Jan 2027 00:00:00 GMT" {
		t.Errorf("Sunset = %q", got)
	}
	if got := header.Get("Link"); got != `</v1/models/acme/new>; rel="successor-version"` {
		t.Errorf("Link = %q", got)
	}
}

// The control: the same call on an active model, an unknown model, a power
// level and no target says nothing, so the headers above measure the lifecycle
// and not the handler.
func TestNothingIsAnnouncedForActiveUnknownOrLevelTargets(t *testing.T) {
	level := contract.PowerLevel("high")
	for name, test := range map[string]struct {
		catalogue *PublicCatalogue
		target    *modelTarget
	}{
		"active":  {lifecycleCatalogue(`{"status":"active"}`), targetOf("acme/old")},
		"unknown": {lifecycleCatalogue(`{"status":"deprecated"}`), targetOf("acme/other")},
		"level":   {lifecycleCatalogue(`{"status":"deprecated"}`), &modelTarget{level: &level}},
		"none":    {lifecycleCatalogue(`{"status":"deprecated"}`), nil},
	} {
		header := http.Header{}
		test.catalogue.announceLifecycle(header, test.target)
		if len(header) != 0 {
			t.Errorf("%s: headers = %v", name, header)
		}
	}
}

func TestARetiredModelWithoutDatesStillSaysSo(t *testing.T) {
	header := http.Header{}
	lifecycleCatalogue(`{"status":"retired"}`).announceLifecycle(header, targetOf("acme/old@r1"))
	if header.Get(HeaderDeprecation) != "true" || header.Get(HeaderSunset) != "" || header.Get("Link") != "" {
		t.Errorf("headers = %v", header)
	}
}
