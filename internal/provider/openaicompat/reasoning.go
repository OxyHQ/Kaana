package openaicompat

import (
	"encoding/json"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/reasoningblock"
)

// reasoning_details is OpenRouter's ordered array for continuing a reasoning
// model across tool turns: each entry (`reasoning.text` with a signature,
// `reasoning.summary`, `reasoning.encrypted`) is sent back on the assistant
// message exactly as it was received, in the same order
// (openrouter.ai/docs/guides/best-practices/reasoning-tokens).
//
// An entry is carried as one reasoning block sealed under FamilyOpenRouter. The
// payload is the entry's own JSON, never reinterpreted: the adapter does not
// know which fields a given upstream requires, so it cannot be allowed to
// drop one. Entries are customer data and are never logged.

// forwardsReasoningDetails reports whether this adapter speaks the dialect whose
// reasoning_details it can send back.
func (a *Adapter) forwardsReasoningDetails() bool {
	return reasoningDialectFor(a.config.Provider) == reasoningObject
}

// detailsAccumulator joins the fragments of a streamed reasoning_details array.
//
// A streamed entry arrives in pieces that share an `index` (the text grows
// across chunks and the signature arrives last), so pieces with the same type
// and index are one entry: `text` is concatenated and every other field takes
// its latest non-null value. An entry with no `index` is its own entry.
type detailsAccumulator struct {
	order   []string
	entries map[string]map[string]any
	loose   int
}

func newDetailsAccumulator() *detailsAccumulator {
	return &detailsAccumulator{entries: make(map[string]map[string]any)}
}

func (d *detailsAccumulator) absorb(choiceIndex int, details []json.RawMessage) {
	for _, raw := range details {
		var entry map[string]any
		if json.Unmarshal(raw, &entry) != nil || len(entry) == 0 {
			continue
		}
		key := ""
		if index, present := entry["index"]; present && index != nil {
			key = fmt.Sprintf("%d/%v/%v", choiceIndex, entry["type"], index)
		} else {
			d.loose++
			key = fmt.Sprintf("%d/loose/%d", choiceIndex, d.loose)
		}
		held, known := d.entries[key]
		if !known {
			d.entries[key] = entry
			d.order = append(d.order, key)
			continue
		}
		for field, value := range entry {
			switch {
			case value == nil:
			case field == "text":
				joined, _ := held[field].(string)
				if text, isText := value.(string); isText {
					held[field] = joined + text
				}
			default:
				held[field] = value
			}
		}
	}
}

// flush emits every accumulated entry as one reasoning block, in the order each
// first appeared, and empties the accumulator.
func (d *detailsAccumulator) flush(out provider.Emitter) error {
	for _, key := range d.order {
		if err := emitReasoningDetail(out, choiceOf(key), d.entries[key]); err != nil {
			return err
		}
	}
	d.order, d.entries = nil, make(map[string]map[string]any)
	return nil
}

func choiceOf(key string) int {
	var choice int
	_, _ = fmt.Sscanf(key, "%d/", &choice)
	return choice
}

func emitReasoningDetail(out provider.Emitter, choiceIndex int, entry map[string]any) error {
	token, err := reasoningblock.Seal(reasoningblock.FamilyOpenRouter, entry)
	if err != nil {
		// An entry that came off the wire as JSON can be encoded again; this is
		// unreachable, and skipping the block loses one continuation rather
		// than a whole answer.
		return nil
	}
	return out.ReasoningBlock(choiceIndex, token)
}

// emitDetailEntries emits a complete (non-streamed) message's reasoning_details
// in order.
func emitDetailEntries(out provider.Emitter, choiceIndex int, details []json.RawMessage) error {
	for _, raw := range details {
		var entry map[string]any
		if json.Unmarshal(raw, &entry) != nil || len(entry) == 0 {
			continue
		}
		if err := emitReasoningDetail(out, choiceIndex, entry); err != nil {
			return err
		}
	}
	return nil
}

// reasoningDetailsFor returns the entries of this dialect among an assistant
// message's reasoning parts, in order, unmodified. Parts of another family, and
// tokens that cannot be read, are dropped.
func reasoningDetailsFor(parts []contract.ContentPart) []json.RawMessage {
	var tokens []string
	for _, part := range parts {
		if part.Type == contract.ContentPartReasoning && part.Opaque != nil {
			tokens = append(tokens, *part.Opaque)
		}
	}
	return reasoningblock.Payloads(reasoningblock.FamilyOpenRouter, tokens)
}

// withoutReasoning removes reasoning parts, which are not content in this
// protocol: they travel as `reasoning_details` or not at all.
func withoutReasoning(parts []contract.ContentPart) []contract.ContentPart {
	kept := make([]contract.ContentPart, 0, len(parts))
	for _, part := range parts {
		if part.Type != contract.ContentPartReasoning {
			kept = append(kept, part)
		}
	}
	return kept
}
