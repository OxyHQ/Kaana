package publicapi

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// nativeModels projects the same declared fields served by the public catalogue.
// Current route evidence supplements unknown capabilities; it never overwrites
// an authoritative negative declaration.
func (c *PublicCatalogue) nativeModels(current *inventory.Inventory, at time.Time, formats FormatKinds) ([]nativeadmission.Model, error) {
	if c == nil || current == nil {
		return nil, nil
	}
	var document struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(c.document, &document); err != nil {
		return nil, fmt.Errorf("publicapi: native declared catalogue: %w", err)
	}
	declarations := make(map[contract.ModelID]json.RawMessage, len(document.Models))
	for _, raw := range document.Models {
		var entry struct {
			ModelID contract.ModelID `json:"modelId"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, err
		}
		declarations[entry.ModelID] = raw
	}
	realtimeKinds := make(map[contract.ModelID][]contract.RealtimeSessionKind)
	for _, entry := range current.Catalogue() {
		if entry.Realtime != nil {
			realtimeKinds[entry.Model] = entry.Realtime.SessionKinds
		}
	}
	result := make([]nativeadmission.Model, 0, len(c.models))
	for id, declared := range c.models {
		var fields struct {
			Capabilities struct {
				InputModalities  []string `json:"inputModalities"`
				OutputModalities []string `json:"outputModalities"`
				Unreported       []string `json:"unreported"`
				Tools            bool     `json:"tools"`
				StructuredOutput bool     `json:"structuredOutput"`
				JSONMode         bool     `json:"jsonMode"`
				Reasoning        bool     `json:"reasoning"`
				Streaming        bool     `json:"streaming"`
				MaxContextTokens int      `json:"maxContextTokens"`
				MaxOutputTokens  int      `json:"maxOutputTokens"`
			} `json:"capabilities"`
			ImageInput struct {
				MaxInputTokensPerImage int `json:"maxInputTokensPerImage"`
			} `json:"imageInput"`
			License struct {
				LicenseID            string `json:"licenseId"`
				CommercialUseAllowed bool   `json:"commercialUseAllowed"`
			} `json:"license"`
			DataPolicy struct {
				RetainsPayloads            bool `json:"retainsPayloads"`
				TrainsOnCustomerData       bool `json:"trainsOnCustomerData"`
				ZeroDataRetentionAvailable bool `json:"zeroDataRetentionAvailable"`
			} `json:"dataPolicy"`
			Pricing struct {
				PriceVersionID string                      `json:"priceVersionId"`
				Currency       contract.CurrencyCode       `json:"currency"`
				UnitPrices     []nativeadmission.UnitPrice `json:"unitPrices"`
			} `json:"pricing"`
			Deprecation struct {
				Status string `json:"status"`
			} `json:"deprecation"`
		}
		raw := declarations[id]
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("publicapi: native catalogue projection: %w", err)
		}
		cap := fields.Capabilities
		model := nativeadmission.Model{ID: id, Audience: declared.audience, PowerClass: declared.powerClass, Active: fields.Deprecation.Status != "retired", Unreported: cap.Unreported, InputModalities: cap.InputModalities, OutputModalities: cap.OutputModalities, Tools: cap.Tools, StructuredOutput: cap.StructuredOutput, JSONMode: cap.JSONMode, Reasoning: cap.Reasoning, Streaming: cap.Streaming, MaxContextTokens: cap.MaxContextTokens, MaxOutputTokens: cap.MaxOutputTokens, MaxInputTokensPerImage: fields.ImageInput.MaxInputTokensPerImage, LicenseID: fields.License.LicenseID, CommercialUseAllowed: fields.License.CommercialUseAllowed, RetainsPayloads: fields.DataPolicy.RetainsPayloads, TrainsOnCustomerData: fields.DataPolicy.TrainsOnCustomerData, ZeroDataRetentionAvailable: fields.DataPolicy.ZeroDataRetentionAvailable, Price: nativeadmission.ModelPrice{ModelLine: id, PriceVersionID: fields.Pricing.PriceVersionID, Currency: fields.Pricing.Currency, UnitPrices: fields.Pricing.UnitPrices}}
		routes, err := current.Resolve(contract.ModelReference(id), at)
		if err != nil {
			continue
		}
		for _, route := range routes.Candidates() {
			for _, format := range []contract.APIFormat{contract.APIFormatAudioSpeech, contract.APIFormatAudioTranscriptions} {
				if nativeFormatServed(formats, route, format) && !slices.Contains(model.APIFormats, string(format)) {
					model.APIFormats = append(model.APIFormats, string(format))
				}
			}
			if route.AcceptedParameters == nil {
				continue
			}
			for _, parameter := range provider.CanonicalParameterSet(*route.AcceptedParameters) {
				if !slices.Contains(model.AcceptedParameters, parameter) {
					model.AcceptedParameters = append(model.AcceptedParameters, parameter)
				}
			}
		}
		model.RealtimeSessionKinds = realtimeKinds[id]
		result = append(result, model)
	}
	slices.SortFunc(result, func(a, b nativeadmission.Model) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return result, nil
}

func nativeFormatServed(formats FormatKinds, route provider.Route, format contract.APIFormat) bool {
	if formats == nil {
		return false
	}
	executes, known := formats.ExecutesFormat(route.Provider, format)
	if !known || !executes {
		return false
	}
	family, classified, expressible := providerconfig.ClassifyModel(route.Provider, route.UpstreamModelID)
	if !classified {
		return true
	}
	if !expressible {
		return false
	}
	// Responses is a public dialect: text adapters translate it to their
	// upstream chat protocol. Model-family classification names that upstream
	// family, not the public dialect accepted by Registry.ExecutesFormat above.
	if format == contract.APIFormatResponses && family.Format == contract.APIFormatChatCompletions && !family.Spoken {
		return true
	}
	return family.Format == format
}
