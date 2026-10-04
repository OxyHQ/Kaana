package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

// PrivateAutoRequest is the v4 wire shape. The ordinary Request stays v2/3.
type PrivateAutoRequest struct {
	Request
	PrivateAutoExecution PrivateAutoExecution `json:"privateAutoExecution"`
}

func (r *PrivateAutoRequest) InferenceRequest() Request {
	request := r.Request
	request.PrivateAutoExecution = &r.PrivateAutoExecution
	return request
}

func (r *PrivateAutoRequest) UnmarshalJSON(raw []byte) error {
	allowed := privateAutoFields(reflect.TypeFor[PrivateAutoRequest]())
	delete(allowed, "scopedExecution")
	if fields, err := privateAutoObject(raw, allowed); err != nil {
		return err
	} else if err = validatePrivateAutoChildren(fields, reflect.TypeFor[PrivateAutoRequest]()); err != nil {
		return err
	}
	type wire PrivateAutoRequest
	var value wire
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*r = PrivateAutoRequest(value)
	request := r.InferenceRequest()
	if err := request.Validate(); err != nil {
		return err
	}
	return request.ValidatePrivateAutoInputBytes(raw)
}

func (r *Request) ValidatePrivateAutoExecution() error {
	s := r.PrivateAutoExecution
	if s == nil {
		if r.SchemaVersion == PrivateAutoRequestEnvelopeVersion {
			return errors.New("contract: private Auto requires its v4 execution")
		}
		return nil
	}
	if s.Validate() != nil || r.SchemaVersion != PrivateAutoRequestEnvelopeVersion || r.ScopedExecution != nil || r.Attribution.UserID != nil || !r.Attribution.HasScope(ScopeInvoke) {
		return errors.New("contract: invalid private Auto envelope")
	}
	p := r.Attribution.Principal
	if s.RequestID != r.Attribution.RequestID || s.Principal.AccountID != string(p.Billing.AccountID) || s.Principal.ApplicationID != string(p.ApplicationID) || s.Principal.CredentialID != string(p.CredentialID) || s.Principal.Environment != p.Environment || s.Policy != r.RoutingPolicy || r.IdempotencyKey == nil || string(*r.IdempotencyKey) != s.OperationID || len(r.AuthorizedRoutes) != 1 || r.Target.Kind != TargetModel || r.Target.ModelReference == nil || *r.Target.ModelReference != s.ModelReference || r.Client.APIFormat != APIFormatDecisions || r.Client.Endpoint != "/internal/auto-classification" {
		return errors.New("contract: private Auto audience differs from signed child")
	}
	route := r.AuthorizedRoutes[0]
	if route.DeploymentID != s.DeploymentID || route.Provider != s.Provider || route.ModelReference != s.ModelReference || route.CustomerProviderCredential != nil || !reflect.DeepEqual(regionSet(route.Regions), regionSet(s.Regions)) || len(route.Regions) != len(s.Regions) {
		return errors.New("contract: private Auto requires its exact platform route")
	}
	received, err := time.Parse(time.RFC3339Nano, string(r.Client.ReceivedAt))
	runtime, _ := parsePrivateAutoTime(s.RuntimeExpiresAt)
	if err != nil || !runtime.After(received) || runtime.After(received.Add(PrivateAutoTimeout)) {
		return errors.New("contract: private Auto child exceeds its short deadline")
	}
	input := r.Input.Decisions
	if r.Input.Format != InputDecisions || input == nil || input.Instructions == nil || *input.Instructions != PrivateAutoInstructions || input.Effort != nil || len(input.State) > PrivateAutoMaximumBytes || len(input.Questions) != 1 {
		return errors.New("contract: private Auto classifier template required")
	}
	q := input.Questions[0]
	if q.ID != "auto-power-level" || q.Kind != "choice" || q.Question != PrivateAutoQuestion || q.Criteria == nil || *q.Criteria != PrivateAutoCriteria || q.Levels != nil || !reflect.DeepEqual(q.Options, []string{"instant", "medium", "high", "xhigh"}) {
		return errors.New("contract: private Auto classifier template differs")
	}
	return nil
}
func regionSet(values []Region) map[Region]bool {
	out := map[Region]bool{}
	for _, v := range values {
		out[v] = true
	}
	return out
}

// Hash exact whole signed input JSON. Never persist/log the transient state.
func (r *Request) ValidatePrivateAutoInputBytes(raw []byte) error {
	if r.PrivateAutoExecution == nil {
		return nil
	}
	allowed := privateAutoFields(reflect.TypeFor[PrivateAutoRequest]())
	delete(allowed, "scopedExecution")
	fields, err := privateAutoObject(raw, allowed)
	if err != nil {
		return err
	}
	input := fields["input"]
	if len(input) > PrivateAutoMaximumBytes {
		return errors.New("contract: private Auto controlled input exceeds byte limit")
	}
	leaf, err := privateAutoObject(input, map[string]bool{"format": true, "decisions": true})
	if err != nil {
		return err
	}
	decisions, err := privateAutoObject(leaf["decisions"], map[string]bool{"state": true, "instructions": true, "questions": true})
	if err != nil {
		return err
	}
	var questions []json.RawMessage
	if err = json.Unmarshal(decisions["questions"], &questions); err != nil {
		return err
	}
	if len(questions) != 1 {
		return errors.New("contract: private Auto single question required")
	}
	if _, err = privateAutoObject(questions[0], map[string]bool{"id": true, "kind": true, "question": true, "criteria": true, "options": true}); err != nil {
		return err
	}
	sum := sha256.Sum256(input)
	if hex.EncodeToString(sum[:]) != r.PrivateAutoExecution.InputSHA256 {
		return errors.New("contract: private Auto input hash differs")
	}
	return nil
}
