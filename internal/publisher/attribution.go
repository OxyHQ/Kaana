package publisher

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// Attribution maps what a PROVIDER calls a model onto the canonical model line
// Oxy's catalogue names it by.
//
// # This file is the boundary, and it is not clean
//
// ADR 0006 gives model catalogue IDENTITY to Oxy and deployment availability to
// Kaana. A deployment row needs both: `upstreamModelId` and `current` are
// execution and belong here, while `modelReference` is identity and belongs
// there. The inventory file is the one artefact that has to carry both, so
// neither side owns it outright — and this table is precisely the half that is
// Oxy's, held here because ADR 0006's "What crosses the boundary" section
// declares exactly one Oxy→Kaana channel, the per-request envelope, and no
// channel by which a catalogue could publish an inventory.
//
// So the rule this file follows is: hold the SMALLEST possible amount of it,
// declaratively, in a form a reviewer can check against a publisher's own
// announcement — and never derive it. Attribution says who RELEASED the
// weights, which is a public fact, and never who serves them. If Oxy ever grows
// a channel that publishes model identity, this table is what it replaces, and
// the deployment half around it does not move.
//
// # Why an unattributed model is dropped rather than guessed
//
// A provider's own list is the authority on what it SERVES and says nothing
// about who released it: Cerebras's `/v1/models` returns `gpt-oss-120b`, not
// `openai/gpt-oss-120b`. Inferring a publisher from the string would put a
// claim about somebody else's work into a customer-visible identifier on the
// strength of a substring. So an id nobody attributed is omitted and named in a
// warning, and the reference simply does not exist until a human adds a line.
type Attribution struct {
	// byProvider is provider slug -> upstream model id -> canonical model line.
	//
	// Keyed per provider because the same weights carry different ids at
	// different providers — `gpt-oss-120b` at Cerebras is `openai/gpt-oss-120b`
	// at OpenRouter — so a single flat table would have to guess which naming
	// it was looking at.
	byProvider map[contract.ProviderSlug]map[string]contract.ModelID
}

// attributionFile is the on-disk shape.
type attributionFile struct {
	// Comment carries the provenance of each line. It is read into a field
	// rather than ignored so that a file whose evidence has been deleted fails
	// review as a diff rather than passing silently.
	Comment     json.RawMessage                          `json:"$comment,omitempty"`
	Attribution map[contract.ProviderSlug]map[string]any `json:"attribution"`
}

// LoadAttribution reads the table, refusing anything a snapshot would later
// have to guess about.
func LoadAttribution(path string) (*Attribution, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("publisher: reading the attribution table %s: %w", path, err)
	}
	return ParseAttribution(raw)
}

// ParseAttribution builds the table from its serialized form.
func ParseAttribution(raw []byte) (*Attribution, error) {
	var parsed attributionFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("publisher: the attribution table is not valid JSON: %w", err)
	}
	if len(parsed.Attribution) == 0 {
		return nil, fmt.Errorf("publisher: the attribution table declares no providers, so every discovered model would be dropped and the snapshot would be empty")
	}

	table := &Attribution{byProvider: make(map[contract.ProviderSlug]map[string]contract.ModelID, len(parsed.Attribution))}
	for slug, models := range parsed.Attribution {
		if !slug.Valid() {
			return nil, fmt.Errorf("publisher: the attribution table names %q, which is not a provider slug", slug)
		}
		if len(models) == 0 {
			return nil, fmt.Errorf("publisher: the attribution table declares provider %q with no models; remove the entry rather than leaving one that attributes nothing", slug)
		}
		resolved := make(map[string]contract.ModelID, len(models))
		for upstreamModelID, value := range models {
			if upstreamModelID == "" {
				return nil, fmt.Errorf("publisher: provider %q attributes an empty upstream model id", slug)
			}
			line, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("publisher: provider %q attributes %q to %v, which is not a model line", slug, upstreamModelID, value)
			}
			modelID := contract.ModelID(line)
			if !modelID.Valid() {
				return nil, fmt.Errorf("publisher: provider %q attributes %q to %q, which is not a <publisher>/<model> model id", slug, upstreamModelID, line)
			}
			resolved[upstreamModelID] = modelID
		}
		table.byProvider[slug] = resolved
	}
	return table, nil
}

// ModelLine returns the canonical model line for a provider's own model id.
//
// The second return is false when nobody attributed it, which the caller turns
// into an omission and a warning — never into a guess.
func (a *Attribution) ModelLine(slug contract.ProviderSlug, upstreamModelID string) (contract.ModelID, bool) {
	models, known := a.byProvider[slug]
	if !known {
		return "", false
	}
	line, attributed := models[upstreamModelID]
	return line, attributed
}

// Providers lists the provider slugs the table attributes anything for, sorted.
// Used to warn about a table entry for a provider nobody discovers, which is
// otherwise invisible: it looks exactly like a provider that serves no models.
func (a *Attribution) Providers() []contract.ProviderSlug {
	slugs := make([]contract.ProviderSlug, 0, len(a.byProvider))
	for slug := range a.byProvider {
		slugs = append(slugs, slug)
	}
	sort.Slice(slugs, func(i, j int) bool { return slugs[i] < slugs[j] })
	return slugs
}
