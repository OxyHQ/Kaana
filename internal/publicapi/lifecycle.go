package publicapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// A model that is deprecated or retired says so on every answer that names it,
// in the standard headers a client or proxy already understands, so a caller
// learns about a sunset from its own traffic instead of from a mailing list.
// The facts are the catalogue's own `deprecation` block (modelDeprecationSchema);
// Kaana never substitutes the replacement (routing.md), it only points to it.
const (
	// HeaderDeprecation is RFC 9745: an sf-date of when the model was deprecated.
	HeaderDeprecation = "Deprecation"
	// HeaderSunset is RFC 8594: the HTTP-date after which the model stops answering.
	HeaderSunset = "Sunset"
)

// lifecycle is the part of a declared deprecation block that is announced.
type lifecycle struct {
	Status                    string `json:"status"`
	AnnouncedAt               string `json:"announcedAt"`
	SunsetAt                  string `json:"sunsetAt"`
	ReplacementModelReference string `json:"replacementModelReference"`
}

// lifecycleOf returns the declared deprecation of a model, or false for an
// unknown or active model.
func (c *PublicCatalogue) lifecycleOf(model contract.ModelReference) (lifecycle, bool) {
	declared, known := c.models[model.ModelID()]
	if !known {
		return lifecycle{}, false
	}
	raw, present := declared.fields["deprecation"]
	if !present {
		return lifecycle{}, false
	}
	var out lifecycle
	if err := json.Unmarshal(raw, &out); err != nil || (out.Status != "deprecated" && out.Status != "retired") {
		return lifecycle{}, false
	}
	return out, true
}

// announceLifecycle stamps Deprecation, Sunset and a successor-version Link on
// a response for a deprecated or retired model. A level, an unknown model or an
// active one adds nothing. Timestamps that do not parse are left out rather
// than guessed.
func (c *PublicCatalogue) announceLifecycle(header http.Header, target *modelTarget) {
	if target == nil || target.reference == nil {
		return
	}
	life, ok := c.lifecycleOf(*target.reference)
	if !ok {
		return
	}
	if at, err := time.Parse(time.RFC3339, life.AnnouncedAt); err == nil {
		header.Set(HeaderDeprecation, "@"+strconv.FormatInt(at.Unix(), 10))
	} else {
		header.Set(HeaderDeprecation, "true")
	}
	if at, err := time.Parse(time.RFC3339, life.SunsetAt); err == nil {
		header.Set(HeaderSunset, at.UTC().Format(http.TimeFormat))
	}
	if replacement := contract.ModelReference(life.ReplacementModelReference); replacement.Valid() {
		if id := string(replacement.ModelID()); strings.Contains(id, "/") {
			header.Add("Link", "</v1/models/"+id+`>; rel="successor-version"`)
		}
	}
}
