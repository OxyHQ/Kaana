package publicapi

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// withhold leaves out of one rendered model every value of the contract's
// CATALOGUE_ADDITIONS that the reader's set lacks. The catalogue's leaves are
// strict and its enums closed, so a value a reader's set does not have makes it
// refuse the whole list, not the one model: GET /v1/models is therefore
// rendered in the set the request states in Kaana-Contract-Version
// (`@kaana.ai/sdk` 2.0.0 and later state it on every request), and a request
// that states none is sent none of the additions. A new value reaches a
// reader once its SDK states a set that has it; adding one to the catalogue
// never refuses a deployed reader's list.
//
// Only the fields an addition names are decoded and re-encoded; the rest of
// the model is left byte for byte as render made it.
func withhold(fields map[string]json.RawMessage, reader contract.ContractSet) error {
	for _, addition := range contract.CatalogueAdditions {
		since, _ := contract.ParseContractSet(addition.Since)
		if !reader.Before(since) {
			continue
		}
		top := addition.Path[0]
		raw, present := fields[top]
		if !present {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		model := map[string]any{top: value}
		if addition.Member == "" {
			withholdField(model, addition.Path)
		} else {
			withholdMember(model, addition, 0)
		}
		value, kept := model[top]
		if !kept {
			delete(fields, top)
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fields[top] = encoded
	}
	return nil
}

// withholdField removes the field at path from value, crossing every list on
// the way.
func withholdField(value any, path []string) {
	switch typed := value.(type) {
	case []any:
		for _, element := range typed {
			withholdField(element, path)
		}
	case map[string]any:
		if len(path) == 1 {
			delete(typed, path[0])
			return
		}
		if child, present := typed[path[0]]; present {
			withholdField(child, path[1:])
		}
	}
}

// withholdMember removes from value, the field at addition.Path[:depth] of a
// model, every list element that holds addition.Member at the end of the
// path. It returns the value to keep, whether value itself holds the member
// (so the list element enclosing it goes), and whether it emptied a list that
// its caller has to answer for: the field at addition.Emptied, removed where
// the walk reaches it.
func withholdMember(value any, addition contract.CatalogueAddition, depth int) (kept any, holds, emptied bool) {
	switch typed := value.(type) {
	case []any:
		remaining := make([]any, 0, len(typed))
		for _, element := range typed {
			keptElement, elementHolds, emptiedBelow := withholdMember(element, addition, depth)
			emptied = emptied || emptiedBelow
			if !elementHolds {
				remaining = append(remaining, keptElement)
			}
		}
		return remaining, false, emptied || (len(typed) > 0 && len(remaining) == 0)
	case map[string]any:
		if depth == len(addition.Path) {
			return typed, false, false
		}
		name := addition.Path[depth]
		child, present := typed[name]
		if !present {
			return typed, false, false
		}
		keptChild, childHolds, childEmptied := withholdMember(child, addition, depth+1)
		if childHolds {
			return typed, true, false
		}
		typed[name] = keptChild
		if childEmptied && slices.Equal(addition.Emptied, addition.Path[:depth+1]) {
			delete(typed, name)
			return typed, false, false
		}
		return typed, false, childEmptied
	case string:
		return typed, depth == len(addition.Path) && typed == addition.Member, false
	}
	return value, false, false
}
