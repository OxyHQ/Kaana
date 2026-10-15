package publicapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

// PublicCatalogue is the part of GET /v1/models Kaana does not measure: who
// published a model, its licence, provenance, data policy, deprecation, power
// class and the price list Oxy bills it at. ADR 0002 gives catalogue identity
// and pricing to Oxy; this is that half, DECLARED in a reviewed file exported
// from Oxy's catalogue (as configs/model-attribution.json is), never derived.
// Kaana adds what it serves: the revisions, the realtime capabilities and the
// voices. A line the inventory serves but this file does not declare is not
// listed, because a model entry without its licence and data policy would
// claim facts nobody stated.
type PublicCatalogue struct {
	models map[contract.ModelID]declaredModel
}

type declaredModel struct {
	audience   contract.CatalogueAudience
	powerClass string
	fields     map[string]json.RawMessage
}

// catalogueFile is the declared file: one entry per model line, in the shape
// of modelSchema without the fields Kaana fills (schemaVersion, the
// revisions), plus its audience. A realtime line's capabilities.realtime and
// audio are Kaana's own reviewed statement (inventory) and replace any
// declared ones; a speech model's audio limits are declared here.
type catalogueFile struct {
	SchemaVersion int                          `json:"schemaVersion"`
	Models        []map[string]json.RawMessage `json:"models"`
}

// Fields the declared file must state for every model: the contract requires
// them and only Oxy knows them.
var declaredRequired = []string{"modelId", "publisher", "displayName", "capabilities", "license", "provenance", "dataPolicy", "deprecation"}

// Fields Kaana fills and the file must not.
var servedFields = []string{"schemaVersion", "currentRevision", "availableRevisions"}

var powerClasses = []string{"instant", "medium", "high", "pro", "ultra"}

// LoadPublicCatalogue reads the declared catalogue. An empty path is an empty
// catalogue: /v1/models then lists nothing rather than invent an entry.
func LoadPublicCatalogue(path string) (*PublicCatalogue, error) {
	catalogue := &PublicCatalogue{models: make(map[contract.ModelID]declaredModel)}
	if strings.TrimSpace(path) == "" {
		return catalogue, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("publicapi: reading the public catalogue: %w", err)
	}
	return ParsePublicCatalogue(raw)
}

// ParsePublicCatalogue validates the declared catalogue.
func ParsePublicCatalogue(raw []byte) (*PublicCatalogue, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file catalogueFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("publicapi: the public catalogue is not readable: %w", err)
	}
	if file.SchemaVersion != 1 {
		return nil, errors.New("publicapi: the public catalogue declares no schemaVersion 1")
	}
	catalogue := &PublicCatalogue{models: make(map[contract.ModelID]declaredModel, len(file.Models))}
	for index, fields := range file.Models {
		for _, required := range declaredRequired {
			if _, present := fields[required]; !present {
				return nil, fmt.Errorf("publicapi: public catalogue models[%d] does not declare %s", index, required)
			}
		}
		for _, served := range servedFields {
			if _, present := fields[served]; present {
				return nil, fmt.Errorf("publicapi: public catalogue models[%d] declares %s, which Kaana serves from its inventory", index, served)
			}
		}
		var line contract.ModelID
		if err := json.Unmarshal(fields["modelId"], &line); err != nil || !line.Valid() {
			return nil, fmt.Errorf("publicapi: public catalogue models[%d].modelId is not publisher/model", index)
		}
		if _, duplicate := catalogue.models[line]; duplicate {
			return nil, fmt.Errorf("publicapi: public catalogue declares %s twice", line)
		}
		model := declaredModel{audience: contract.AudiencePublic, fields: fields}
		if raw, present := fields["audience"]; present {
			if err := json.Unmarshal(raw, &model.audience); err != nil ||
				(model.audience != contract.AudiencePublic && model.audience != contract.AudiencePlatformInternal) {
				return nil, fmt.Errorf("publicapi: public catalogue models[%d].audience is public or platform_internal", index)
			}
			delete(fields, "audience")
		}
		if raw, present := fields["powerClass"]; present {
			if err := json.Unmarshal(raw, &model.powerClass); err != nil || !slices.Contains(powerClasses, model.powerClass) {
				return nil, fmt.Errorf("publicapi: public catalogue models[%d].powerClass is not a power class", index)
			}
		}
		catalogue.models[line] = model
	}
	return catalogue, nil
}

// visible reports whether an audience may see a declared model.
func (m declaredModel) visible(audience contract.CatalogueAudience) bool {
	return m.audience == contract.AudiencePublic || audience == contract.AudiencePlatformInternal
}

// render is one modelSchema entry: the declaration, with what Kaana serves.
func (m declaredModel) render(entry inventory.CatalogueEntry, revisions []string) (json.RawMessage, error) {
	fields := make(map[string]json.RawMessage, len(m.fields)+4)
	for name, value := range m.fields {
		fields[name] = value
	}
	_, current, _ := strings.Cut(string(entry.Reference), "@")
	set := func(name string, value any) error {
		encoded, err := json.Marshal(value)
		fields[name] = encoded
		return err
	}
	if err := errors.Join(set("schemaVersion", 1), set("currentRevision", current), set("availableRevisions", revisions)); err != nil {
		return nil, err
	}
	if entry.Realtime != nil {
		var capabilities map[string]json.RawMessage
		if err := json.Unmarshal(fields["capabilities"], &capabilities); err != nil {
			return nil, err
		}
		realtime := map[string]any{"transports": entry.Realtime.Transports, "sessionKinds": entry.Realtime.SessionKinds,
			"inputAudioTranscription": entry.Realtime.InputAudioTranscription, "audioFormats": entry.Realtime.AudioFormats}
		encoded, err := json.Marshal(realtime)
		if err != nil {
			return nil, err
		}
		capabilities["realtime"] = encoded
		if fields["capabilities"], err = json.Marshal(capabilities); err != nil {
			return nil, err
		}
	}
	if entry.Audio != nil {
		voices := make([]map[string]any, 0, len(entry.Audio.Voices))
		for _, voice := range entry.Audio.Voices {
			rendered := map[string]any{"id": voice.ID, "name": voice.Name, "languages": voice.Languages}
			if voice.Gender != "" {
				rendered["gender"] = voice.Gender
			}
			voices = append(voices, rendered)
		}
		// A realtime line names no file media types: its encodings are
		// capabilities.realtime.audioFormats.
		if err := set("audio", map[string]any{"voices": voices, "languages": entry.Audio.Languages,
			"audioFormats": map[string]any{"input": []string{}, "output": []string{}}}); err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

// Audio implements AudioCatalogue from the declared audio limits of a speech
// model (ADR 0002, decision 9): what the speech handler checks before a grant.
func (c *PublicCatalogue) Audio(model contract.ModelReference) (*AudioCapabilities, bool) {
	declared, known := c.models[model.ModelID()]
	if !known {
		return nil, false
	}
	raw, present := declared.fields["audio"]
	if !present {
		return nil, false
	}
	var audio struct {
		Voices []struct {
			ID string `json:"id"`
		} `json:"voices"`
		Languages    []string `json:"languages"`
		AudioFormats struct {
			Input  []contract.AudioInputMediaType `json:"input"`
			Output []contract.AudioMediaType      `json:"output"`
		} `json:"audioFormats"`
		Speed              *SpeedRange `json:"speed"`
		MaxInputCharacters *int        `json:"maxInputCharacters"`
	}
	if json.Unmarshal(raw, &audio) != nil {
		return nil, false
	}
	capabilities := &AudioCapabilities{Languages: audio.Languages, InputMediaTypes: audio.AudioFormats.Input,
		OutputMediaTypes: audio.AudioFormats.Output, Speed: audio.Speed, MaxInputCharacters: audio.MaxInputCharacters}
	for _, voice := range audio.Voices {
		capabilities.Voices = append(capabilities.Voices, voice.ID)
	}
	return capabilities, true
}

/* -------------------------------------------------------------------------- */
/*  GET /v1/models and GET /v1/power-levels                                   */
/* -------------------------------------------------------------------------- */

// catalogueAccess asks Oxy what the caller may see (catalogue.read, cached by
// the authorizer for at most 60 seconds per credential).
func (s *Server) catalogueAccess(r *http.Request, requestID contract.RequestID) (*contract.CatalogueAccess, *contract.PublicError) {
	bearer, ok := bearerFrom(r)
	if !ok {
		return nil, contract.NewPublicError(requestID, "authentication_failed", contract.PublicMessage("authentication_failed"))
	}
	if wait, allowed := s.limiter.allow(bearer.digest); !allowed {
		return nil, contract.NewPublicError(requestID, "rate_limited", contract.PublicMessage("rate_limited")).WithRetryAfter(int(wait.Milliseconds()) + 1)
	}
	return s.authorizer.Catalogue(r.Context(), contract.CatalogueAuthorizationRequest{
		SchemaVersion: contract.AuthorizationSchemaVersion, Kind: contract.AuthorizationKindCatalogue,
		Credential: bearer.value, RequestID: requestID,
	})
}

// served is every line the inventory serves now that the catalogue declares
// and the audience may see, with its served revisions.
func (s *Server) served(audience contract.CatalogueAudience) []servedModel {
	current := s.inventory.Current()
	revisions := make(map[contract.ModelID][]string)
	for _, endpoint := range current.PublicDeployments() {
		route, err := current.Deployment(endpoint.DeploymentID)
		if err != nil {
			continue
		}
		if _, revision, pinned := strings.Cut(string(route.ModelReference), "@"); pinned {
			line := route.ModelReference.ModelID()
			if !slices.Contains(revisions[line], revision) {
				revisions[line] = append(revisions[line], revision)
			}
		}
	}
	var models []servedModel
	for _, entry := range current.Catalogue() {
		declared, known := s.catalogue.models[entry.Model]
		if !known || !declared.visible(audience) {
			continue
		}
		lineRevisions := revisions[entry.Model]
		sort.Strings(lineRevisions)
		models = append(models, servedModel{entry: entry, declared: declared, revisions: lineRevisions})
	}
	return models
}

type servedModel struct {
	entry     inventory.CatalogueEntry
	declared  declaredModel
	revisions []string
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	requestID := NewRequestID()
	access, failure := s.catalogueAccess(r, requestID)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	data := make([]json.RawMessage, 0)
	for _, model := range s.served(access.Audience) {
		rendered, err := model.declared.render(model.entry, model.revisions)
		if err != nil {
			writeError(w, requestID, contract.NewPublicError(requestID, "internal_error", contract.PublicMessage("internal_error")))
			return
		}
		data = append(data, rendered)
	}
	writeJSON(w, requestID, http.StatusOK, map[string]any{"data": data})
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	requestID := NewRequestID()
	reference := contract.ModelReference(r.PathValue("publisher") + "/" + r.PathValue("model"))
	if !reference.Valid() {
		writeError(w, requestID, invalid(requestID, "model", "the path names publisher/model[@revision]"))
		return
	}
	access, failure := s.catalogueAccess(r, requestID)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	_, revision, pinned := strings.Cut(string(reference), "@")
	for _, model := range s.served(access.Audience) {
		if model.entry.Model != reference.ModelID() || (pinned && !slices.Contains(model.revisions, revision)) {
			continue
		}
		rendered, err := model.declared.render(model.entry, model.revisions)
		if err != nil {
			break
		}
		writeJSON(w, requestID, http.StatusOK, rendered)
		return
	}
	writeError(w, requestID, contract.NewPublicError(requestID, "model_not_found", contract.PublicMessage("model_not_found")).WithParam("model"))
}

// levelClass is the power class each level draws from: xhigh draws from the
// high class (Oxy's POWER_LEVEL_CLASS), auto from none of its own.
var levelClass = map[contract.PowerLevel]string{
	contract.PowerInstant: "instant", contract.PowerMedium: "medium", contract.PowerHigh: "high",
	contract.PowerXHigh: "high", contract.PowerPro: "pro", contract.PowerUltra: "ultra",
}

// autoClasses are the classes `auto` chooses among: its ceiling is xhigh.
var autoClasses = []string{"instant", "medium", "high"}

type powerLevelEntry struct {
	Level      contract.PowerLevel `json:"level"`
	PowerClass *string             `json:"powerClass,omitempty"`
	ModelLines []contract.ModelID  `json:"modelLines"`
}

func (s *Server) handlePowerLevels(w http.ResponseWriter, r *http.Request) {
	requestID := NewRequestID()
	access, failure := s.catalogueAccess(r, requestID)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	served := s.served(access.Audience)
	linesOf := func(classes ...string) []contract.ModelID {
		lines := make([]contract.ModelID, 0)
		for _, model := range served {
			if slices.Contains(classes, model.declared.powerClass) {
				lines = append(lines, model.entry.Model)
			}
		}
		return lines
	}
	data := make([]powerLevelEntry, 0, 7)
	for _, level := range []contract.PowerLevel{contract.PowerAuto, contract.PowerInstant, contract.PowerMedium, contract.PowerHigh, contract.PowerXHigh, contract.PowerPro, contract.PowerUltra} {
		if len(access.AllowedLevels) > 0 && !slices.Contains(access.AllowedLevels, level) {
			continue
		}
		if level == contract.PowerAuto {
			data = append(data, powerLevelEntry{Level: level, ModelLines: linesOf(autoClasses...)})
			continue
		}
		class := levelClass[level]
		data = append(data, powerLevelEntry{Level: level, PowerClass: &class, ModelLines: linesOf(class)})
	}
	writeJSON(w, requestID, http.StatusOK, map[string]any{"data": data})
}

/* -------------------------------------------------------------------------- */
/*  GET /v1/generations                                                       */
/* -------------------------------------------------------------------------- */

func (s *Server) handleGeneration(w http.ResponseWriter, r *http.Request) {
	requestID := NewRequestID()
	generation := contract.GenerationID(r.PathValue("id"))
	if generation == "" || len(generation) > 128 {
		writeError(w, requestID, invalid(requestID, "id", "a generation id is 1 to 128 characters"))
		return
	}
	s.receipt(w, r, requestID, contract.ReceiptAuthorizationRequest{GenerationID: &generation})
}

func (s *Server) handleGenerationByKey(w http.ResponseWriter, r *http.Request) {
	requestID := NewRequestID()
	query := r.URL.Query()
	key := contract.IdempotencyKey(query.Get("idempotency_key"))
	if len(query) != 1 || key == "" || len(key) > maxIdempotencyKeyLength {
		writeError(w, requestID, invalid(requestID, "idempotency_key", "GET /v1/generations takes exactly ?idempotency_key="))
		return
	}
	user, ok := onBehalfOf(r)
	if !ok {
		writeError(w, requestID, invalid(requestID, HeaderOnBehalfOf, "X-Oxy-User-Id is 1 to 64 characters without whitespace"))
		return
	}
	s.receipt(w, r, requestID, contract.ReceiptAuthorizationRequest{IdempotencyKey: &key, OnBehalfOfUserID: user})
}

// receipt relays the generation record Oxy answers a receipt.read with. The
// record is Oxy's, amount included; Kaana passes it through unread.
func (s *Server) receipt(w http.ResponseWriter, r *http.Request, requestID contract.RequestID, request contract.ReceiptAuthorizationRequest) {
	bearer, ok := bearerFrom(r)
	if !ok {
		writeError(w, requestID, contract.NewPublicError(requestID, "authentication_failed", contract.PublicMessage("authentication_failed")))
		return
	}
	if wait, allowed := s.limiter.allow(bearer.digest); !allowed {
		writeError(w, requestID, contract.NewPublicError(requestID, "rate_limited", contract.PublicMessage("rate_limited")).WithRetryAfter(int(wait.Milliseconds())+1))
		return
	}
	request.SchemaVersion, request.Kind, request.Credential, request.RequestID = contract.AuthorizationSchemaVersion, contract.AuthorizationKindReceipt, bearer.value, requestID
	record, failure := s.authorizer.Receipt(r.Context(), request)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	w.Header().Set(HeaderRequestID, string(requestID))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(record)
}

/* -------------------------------------------------------------------------- */
/*  POST /v1/images/generations                                               */
/* -------------------------------------------------------------------------- */

// imageRequest is imageGenerationsRequestSchema.
type imageRequest struct {
	Model          contract.ModelReference `json:"model"`
	Prompt         string                  `json:"prompt"`
	N              *int                    `json:"n,omitempty"`
	Size           *string                 `json:"size,omitempty"`
	Quality        *string                 `json:"quality,omitempty"`
	Style          *string                 `json:"style,omitempty"`
	ResponseFormat *string                 `json:"response_format,omitempty"`
	User           *string                 `json:"user,omitempty"`
}

// handleImages validates an image request and refuses it before any grant:
// no Kaana adapter executes image generation, and the signed envelope has no
// image-generation input, so nothing could serve it. Refusing before Oxy is
// asked holds no money for a request that cannot run.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	requestID := NewRequestID()
	body, failure := s.readJSON(w, r, requestID)
	if failure != nil {
		writeError(w, requestID, failure)
		return
	}
	var request imageRequest
	if failure := decodeRequest(body, requestID, &request); failure != nil {
		writeError(w, requestID, failure)
		return
	}
	switch {
	case !request.Model.Valid():
		failure = invalid(requestID, "model", "model is a model reference (publisher/model[@revision])")
	case request.Prompt == "" || utf16Length(request.Prompt) > 32000:
		failure = invalid(requestID, "prompt", "prompt is 1 to 32000 characters")
	case request.N != nil && (*request.N < 1 || *request.N > 10):
		failure = invalid(requestID, "n", "n is 1 to 10")
	case request.ResponseFormat != nil && *request.ResponseFormat != "url" && *request.ResponseFormat != "b64_json":
		failure = invalid(requestID, "response_format", "response_format is url or b64_json")
	default:
		failure = contract.NewPublicError(requestID, "unsupported_modality", "no model is served for image generation").WithParam("model")
	}
	writeError(w, requestID, failure)
}
