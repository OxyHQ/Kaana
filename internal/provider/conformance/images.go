package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

// ImagesScenario names what a fake text-to-image upstream does with the one
// request the suite sends it (ImagesRequest).
type ImagesScenario string

const (
	// ImagesAnswered answers with ImagesSubject.Images images and reports
	// ImagesSubject.InputTokens and OutputTokens when it reports usage at all.
	ImagesAnswered ImagesScenario = "answered"
	// ImagesEmpty answers 200 with an empty image list.
	ImagesEmpty ImagesScenario = "empty"
	// ImagesRateLimited refuses with a transient rate limit.
	ImagesRateLimited ImagesScenario = "rate_limited"
	// ImagesServerError refuses with the provider's own 500.
	ImagesServerError ImagesScenario = "server_error"
	// ImagesRequestRefused refuses the request itself (400) after accepting
	// the credential.
	ImagesRequestRefused ImagesScenario = "request_refused"
	// ImagesHeld reads the request and answers nothing until its caller
	// goes away.
	ImagesHeld ImagesScenario = "held"
)

// ImagesUpstream is a running fake text-to-image provider.
type ImagesUpstream struct {
	URL string
	// Requests is how many HTTP requests it received.
	Requests func() int
	// Disconnected reports whether it saw its caller go away mid-request.
	Disconnected func() bool
	// Arrived is closed when the upstream has read a request.
	Arrived <-chan struct{}
}

// ImagesSubject is what an images adapter author supplies.
type ImagesSubject struct {
	Name            string
	Provider        contract.ProviderSlug
	ModelReference  contract.ModelReference
	UpstreamModelID string
	// Images is how many images ImagesAnswered returns (at least two, so a
	// count of one cannot pass for the right one).
	Images int
	// InputTokens and OutputTokens are what the fake reports; zero for a
	// provider that reports no tokens for an image.
	InputTokens, OutputTokens int
	// NewAdapter builds the adapter pointed at a fake upstream.
	NewAdapter func(t *testing.T, upstreamURL string) provider.Adapter
	// StartUpstream starts a fake speaking the provider's real wire. It
	// asserts the request it receives is the translation of ImagesRequest.
	StartUpstream func(t *testing.T, scenario ImagesScenario) *ImagesUpstream
}

// ImagesPrompt is the prompt every subject is asked.
const ImagesPrompt = "a synthetic lighthouse at dusk"

// ImagesRequest is the request every subject is sent: the prompt and every
// control the envelope carries.
func ImagesRequest(subject ImagesSubject) *contract.Request {
	reference := subject.ModelReference
	prompt, count, size, quality, style := ImagesPrompt, subject.Images, "1024x1024", "high", "vivid"
	format := contract.ImageResponseB64JSON
	return &contract.Request{
		SchemaVersion: contract.RequestEnvelopeVersion,
		Attribution: contract.Attribution{
			Principal: contract.AuthenticatedPrincipal{
				Billing: contract.BillingPrincipal{AccountID: "acc_conformance"}, ApplicationID: "app_conformance",
				CredentialID: "cred_conformance", Environment: contract.EnvironmentDevelopment,
				InferenceScopes: []contract.Scope{contract.ScopeInvoke},
			},
			RequestID: "req_conformance_images",
		},
		Target:   contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference},
		Modality: contract.ModalityImage,
		Input:    contract.Input{Format: contract.InputText, Text: &prompt},
		Image:    &contract.ImageParameters{N: &count, Size: &size, Quality: &quality, Style: &style, ResponseFormat: &format},
		Client: contract.ClientRequestMetadata{
			APIFormat: contract.APIFormatImagesGenerations, Endpoint: "/v1/images/generations",
			ReceivedAt: contract.NewTimestamp(time.Now()),
		},
		RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: "rp_conformance", PolicyVersion: 1},
		AuthorizedRoutes: []contract.AuthorizedRoute{{
			Substitution: contract.SubstitutionSameModel, DeploymentID: "dep_conformance",
			ModelReference: reference, Provider: subject.Provider, Regions: []contract.Region{"test-region"},
		}},
	}
}

// RunImages holds a text-to-image adapter to what the platform depends on,
// driven through the real executor on one deployment: a complete answer with
// its images and units; an answer with no image, which is the deployment's
// failure and bills nothing; the provider's throttle, fault and refusal
// classified as such; and a cancellation that reaches the provider.
func RunImages(t *testing.T, subject ImagesSubject) {
	t.Helper()
	if subject.Images < 2 || subject.NewAdapter == nil || subject.StartUpstream == nil || !subject.ModelReference.Pinned() {
		t.Fatal("an images subject needs at least two images, an adapter, a fake upstream and a pinned model")
	}

	t.Run("serves every image with its units", func(t *testing.T) {
		r := executeImages(t, subject, ImagesAnswered, nil)
		if r.result.Failure != nil || r.result.Image == nil || r.result.Report == nil {
			t.Fatalf("an answered request failed: %+v", r.result.Failure)
		}
		if got := len(r.result.Image.Images); got != subject.Images {
			t.Errorf("%d images served, the upstream returned %d", got, subject.Images)
		}
		report := r.result.Report
		if report.Outcome != contract.OutcomeCompleted || report.UsageSource != contract.UsageProviderReported {
			t.Errorf("report = %s from %s", report.Outcome, report.UsageSource)
		}
		if report.ResolvedModelReference != subject.ModelReference {
			t.Errorf("the report names %q, the deployment serves %q", report.ResolvedModelReference, subject.ModelReference)
		}
		units := map[contract.UsageUnit]int{}
		for _, unit := range report.Units {
			units[unit.Unit] = unit.Quantity
		}
		if units[contract.UnitImages] != subject.Images || units[contract.UnitInputTokens] != subject.InputTokens || units[contract.UnitOutputTokens] != subject.OutputTokens {
			t.Errorf("units = %v; want %d images, %d input and %d output tokens", units, subject.Images, subject.InputTokens, subject.OutputTokens)
		}
		if len(r.result.UpstreamCost.Attempts) != 1 || !r.result.UpstreamCost.Attempts[0].Served {
			t.Errorf("cost attempts = %+v; want one served attempt", r.result.UpstreamCost.Attempts)
		}
		if r.upstream.Requests() != 1 {
			t.Errorf("one answer took %d upstream requests", r.upstream.Requests())
		}
		if len(r.events) != 0 {
			t.Errorf("an image answer is a whole response, yet %d stream events were emitted", len(r.events))
		}
	})

	t.Run("an answer with no image is a failure that bills nothing", func(t *testing.T) {
		r := executeImages(t, subject, ImagesEmpty, nil)
		if r.result.Image != nil {
			t.Error("an empty answer was served")
		}
		failure := r.result.Failure
		if failure == nil || failure.Code != contract.CodeProviderError || !failure.Retryable {
			t.Fatalf("failure = %+v; an attempt that produced no image is a retryable provider error", failure)
		}
		assertImagesBilledNothing(t, r)
	})

	t.Run("a throttle is a rate limit", func(t *testing.T) {
		r := executeImages(t, subject, ImagesRateLimited, nil)
		if r.result.Failure == nil || r.result.Failure.Code != contract.CodeRateLimited || r.result.Image != nil {
			t.Fatalf("failure = %+v", r.result.Failure)
		}
		assertImagesBilledNothing(t, r)
	})

	t.Run("a provider fault is a provider error", func(t *testing.T) {
		r := executeImages(t, subject, ImagesServerError, nil)
		if r.result.Failure == nil || r.result.Failure.Code != contract.CodeProviderError || r.result.Image != nil {
			t.Fatalf("failure = %+v", r.result.Failure)
		}
		assertImagesBilledNothing(t, r)
	})

	t.Run("a refused request is the caller's, not retryable", func(t *testing.T) {
		r := executeImages(t, subject, ImagesRequestRefused, nil)
		if r.result.Failure == nil || r.result.Failure.Retryable || r.result.Failure.Code == contract.CodeProviderError || r.result.Image != nil {
			t.Fatalf("failure = %+v", r.result.Failure)
		}
		assertImagesBilledNothing(t, r)
		if r.upstream.Requests() != 1 {
			t.Errorf("a refusal every key would repeat took %d upstream requests", r.upstream.Requests())
		}
	})

	t.Run("cancelling reaches the provider and ends as a cancellation", func(t *testing.T) {
		r := executeImages(t, subject, ImagesHeld, func(upstream *ImagesUpstream, cancel context.CancelFunc) {
			select {
			case <-upstream.Arrived:
				cancel()
			case <-time.After(10 * time.Second):
				cancel()
			}
		})
		if r.result.Failure == nil || r.result.Failure.Code != contract.CodeCancelled || r.result.Image != nil {
			t.Fatalf("failure = %+v", r.result.Failure)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !r.upstream.Disconnected() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !r.upstream.Disconnected() {
			t.Error("the provider never saw its caller go away")
		}
	})
}

func assertImagesBilledNothing(t *testing.T, r *imagesRun) {
	t.Helper()
	report := r.result.Report
	if report == nil || report.Outcome != contract.OutcomeFailed || len(report.Units) != 0 {
		t.Errorf("report = %+v; a failed image attempt reports no units", report)
	}
	for _, attempt := range r.result.UpstreamCost.Attempts {
		if attempt.Served {
			t.Errorf("attempt %d is marked served", attempt.AttemptIndex)
		}
	}
}

type imagesRun struct {
	result   kaana.Result
	events   []contract.StreamEvent
	upstream *ImagesUpstream
	cost     providercost.Record
}

func executeImages(t *testing.T, subject ImagesSubject, scenario ImagesScenario, act func(*ImagesUpstream, context.CancelFunc)) *imagesRun {
	t.Helper()
	upstream := subject.StartUpstream(t, scenario)
	adapter := subject.NewAdapter(t, upstream.URL)
	inventoryJSON := fmt.Sprintf(`{"snapshotId":"snap_conformance","issuedAt":%q,"deployments":[{"deploymentId":"dep_conformance","provider":%q,"modelReference":%q,"upstreamModelId":%q,"regions":["test-region"],"current":true}]}`,
		contract.NewTimestamp(time.Now()), subject.Provider, subject.ModelReference, subject.UpstreamModelID)
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(path, []byte(inventoryJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := inventory.NewStore(inventory.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := kaana.NewExecutor(kaana.Config{
		Inventory: store, Providers: registry, Rotation: rotation.NewRegistry(rotation.Policy{}, nil),
		Retry: &kaana.RetryPolicy{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &imagesRun{upstream: upstream}
	if act != nil {
		go act(upstream, cancel)
	}
	run.result = executor.Execute(ctx, ImagesRequest(subject), func(event contract.StreamEvent) error {
		run.events = append(run.events, event)
		return nil
	})
	run.cost = run.result.UpstreamCost
	return run
}
