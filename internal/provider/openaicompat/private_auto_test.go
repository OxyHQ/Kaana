package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrivateAutoActualAdapterBindsTemplateQuoteKeyAndSingleSend(t *testing.T) {
	raw, err := os.ReadFile("../../contract/testdata/private-auto/golden-first.json")
	if err != nil {
		t.Fatal(err)
	}
	var wire contract.PrivateAutoRequest
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	r := wire.InferenceRequest()
	raw, err = os.ReadFile("../../contract/testdata/private-auto/golden-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	var source contract.PrivateAutoSourceApproval
	if err = json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	source.ModelReference = "typesafe/jev-1.13@2026-09-17"
	source.UpstreamModelID = "typesafe/jev-1.13-20260917"
	a, err := New(Config{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatal(err)
	}
	source.KeyID, _ = a.PlatformCredentials().SoleKeyID()
	r.PrivateAutoExecution.PrivateAutoAuthority = source.PrivateAutoAuthority
	r.PrivateAutoExecution.ApprovalSHA256, _ = source.SHA256()
	r.PrivateAutoExecution.RuntimeExpiresAt = time.Now().UTC().Truncate(time.Millisecond).Add(time.Second).UTC().Format(time.RFC3339Nano)
	r.Client.ReceivedAt = contract.NewTimestamp(time.Now())
	r.Target.ModelReference = &source.ModelReference
	r.AuthorizedRoutes[0].ModelReference = source.ModelReference
	prompt, _ := providercost.ParseDecisionTokenPrice("0.042")
	route := provider.Route{PrivateAutoSourceApproval: &source, ScopedDecisionPriceLimit: &providercost.DecisionPriceLimit{Prompt: prompt}, DeploymentID: source.DeploymentID, Provider: source.Provider, ModelReference: source.ModelReference, UpstreamModelID: source.UpstreamModelID, Regions: []contract.Region{}}
	var sends atomic.Int32
	var claimed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sends.Add(1)
		if !claimed {
			t.Error("provider before claim")
		}
		var body systemOneRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != source.UpstreamModelID || len(body.Questions) != 1 || body.Questions["auto-power-level"].Type != "choice" {
			t.Error("template changed")
		}
		if body.Provider.AllowFallbacks || !body.Provider.ZDR || body.Provider.DataCollection != "deny" {
			t.Error("gateway broadened")
		}
		_, _ = io.WriteString(w, `{"id":"synthetic-private-auto","model":"typesafe/jev-1.13-20260917","provider":"TypeSafe","answers":{"auto-power-level":{"type":"choice","choice":"instant","confidence":1,"probabilities":{"instant":1,"medium":0,"high":0,"xhigh":0}}},"usage":{"input_tokens":10,"output_tokens":0,"cost":0.00000042}}`)
	}))
	defer server.Close()
	if _, err = a.Translate(&r, route); err == nil {
		t.Fatal("nil source admitted")
	}
	a.privateAutoSource = func() *contract.PrivateAutoSourceApproval { return &source }
	a.scopedHTTPClient = func() *http.Client { return identityBoundFakeClient(t, server.URL) }
	if e := r.ValidatePrivateAutoExecution(); e != nil {
		t.Fatal(e)
	}
	if !r.PrivateAutoExecution.MatchesSource(&source, time.Now()) {
		t.Fatal("source does not match child", source.Validate(), r.PrivateAutoExecution.Validate())
	}
	call, err := a.Translate(&r, route)
	if err != nil {
		t.Fatal(err)
	}
	if a.decisions.approved() {
		t.Fatal("ordinary rights changed")
	}
	if _, err = a.Stream(context.Background(), call, nil, nil); err == nil || sends.Load() != 0 {
		t.Fatal("missing durable claim sent")
	}
	call.ScopedAttempt = &provider.ScopedCredentialAttempt{KeyID: source.KeyID, Claim: func(context.Context) error {
		if claimed {
			return errors.New("consumed")
		}
		claimed = true
		return nil
	}}
	pool, err := a.PlatformCredentials().Bind(source.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := a.Stream(context.Background(), call, nil, pool)
	if err != nil || len(outcome.Decisions) != 1 || sends.Load() != 1 {
		t.Fatal("positive failed", err, sends.Load())
	}
	if _, err = a.Stream(context.Background(), call, nil, pool); err == nil || sends.Load() != 1 {
		t.Fatal("replayed provider exchange")
	}
	r.PrivateAutoExecution.RuntimeExpiresAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	if _, err = a.Translate(&r, route); err == nil {
		t.Fatal("expired runtime translated")
	}
	r.PrivateAutoExecution.RuntimeExpiresAt = time.Now().UTC().Truncate(time.Millisecond).Add(time.Second).UTC().Format(time.RFC3339Nano)
	r.Client.ReceivedAt = contract.NewTimestamp(time.Now())
	source.Review.CommercialUseAllowed = false
	expensive, _ := providercost.ParseDecisionTokenPrice("42")
	route.ScopedDecisionPriceLimit = &providercost.DecisionPriceLimit{Prompt: expensive}
	if _, err = a.Translate(&r, route); err == nil {
		t.Fatal("actual quote exceeded source ceiling")
	}
	r.Input.Decisions.Instructions = ptrString("override " + strings.Repeat("x", 2))
	if _, err = a.Translate(&r, route); err == nil {
		t.Fatal("caller template accepted")
	}
}
func ptrString(v string) *string { return &v }
