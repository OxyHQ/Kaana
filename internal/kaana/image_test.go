package kaana_test

import (
	"context"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func imageRequest() *contract.Request {
	request := authorizedRequest()
	prompt := "a synthetic lighthouse"
	request.Modality = contract.ModalityImage
	request.Input = contract.Input{Format: contract.InputText, Text: &prompt}
	request.Stream = false
	request.Client.APIFormat = contract.APIFormatImagesGenerations
	request.Client.Endpoint = "/v1/images/generations"
	return request
}

// imageAdapter answers an image request with the given outcome and error.
func imageAdapter(slug contract.ProviderSlug, outcome provider.Outcome, failure error) *scriptedAdapter {
	return &scriptedAdapter{slug: slug, stream: func(context.Context, *provider.Call, provider.Emitter) (provider.Outcome, error) {
		return outcome, failure
	}}
}

func producedImages(count int) provider.Outcome {
	images := make([]provider.GeneratedImage, count)
	for index := range images {
		images[index] = provider.GeneratedImage{B64JSON: "AAAA"}
	}
	return provider.Outcome{
		Image:       &provider.ImageResult{Images: images},
		Units:       []contract.UsageQuantity{{Unit: contract.UnitImages, Quantity: count}, {Unit: contract.UnitOutputTokens, Quantity: 1000}},
		UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop,
	}
}

func TestAnImageRequestIsServedWithItsImagesAndUnits(t *testing.T) {
	primary := imageAdapter("stub", producedImages(2), nil)
	_, result := harness{deployments: twoDeploymentsOfOneRevision, adapters: []provider.Adapter{primary, succeedingAdapter("backup", 1)}, retry: &noRetries}.run(t, imageRequest())
	if result.Failure != nil || result.Image == nil || len(result.Image.Images) != 2 {
		t.Fatalf("result = %+v", result)
	}
	report := result.Report
	if report == nil || report.Outcome != contract.OutcomeCompleted || report.UsageSource != contract.UsageProviderReported || report.DeploymentID != "dep_a" {
		t.Fatalf("report = %+v", report)
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(report.Units) != 2 || report.Units[0].Unit != contract.UnitImages || report.Units[0].Quantity != 2 {
		t.Errorf("units = %+v", report.Units)
	}
	attempts := result.UpstreamCost.Attempts
	if len(attempts) != 1 || !attempts[0].Served || attempts[0].Units[0].Unit != contract.UnitImages {
		t.Errorf("cost attempts = %+v; want one served row carrying the units", attempts)
	}
}

// An adapter that reports success without a single image has produced nothing.
// The attempt is the deployment's failure, not an empty list served and
// billed (#266): the customer is told provider_error, reported no units, and
// the attempt is an unserved cost row.
func TestAnImageAttemptThatProducedNoImageIsAFailureNotAnAnswer(t *testing.T) {
	for name, outcome := range map[string]provider.Outcome{
		"no image member": {Units: []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 12}}, UsageSource: contract.UsageProviderReported},
		"an empty list":   {Image: &provider.ImageResult{}, Units: []contract.UsageQuantity{{Unit: contract.UnitImages, Quantity: 0}, {Unit: contract.UnitInputTokens, Quantity: 12}}, UsageSource: contract.UsageProviderReported},
	} {
		t.Run(name, func(t *testing.T) {
			adapter := imageAdapter("stub", outcome, nil)
			_, result := harness{deployments: twoDeploymentsOfOneRevision, adapters: []provider.Adapter{adapter, succeedingAdapter("backup", 1)}, retry: &noRetries}.run(t, withSingleRoute(imageRequest()))
			if result.Image != nil {
				t.Fatalf("an attempt that produced no image was served: %+v", result.Image)
			}
			if result.Failure == nil || result.Failure.Code != contract.CodeProviderError || !result.Failure.Retryable {
				t.Fatalf("failure = %+v", result.Failure)
			}
			if result.Report == nil || result.Report.Outcome != contract.OutcomeFailed || len(result.Report.Units) != 0 {
				t.Errorf("report = %+v; the customer is billed nothing", result.Report)
			}
			attempts := result.UpstreamCost.Attempts
			if len(attempts) != 1 || attempts[0].Served {
				t.Errorf("cost attempts = %+v; want one unserved row", attempts)
			}
		})
	}
}

// The empty attempt is attributable like any server error, so the request
// moves to the next authorized route and is served there.
func TestAnEmptyImageAttemptFailsOverToTheNextRoute(t *testing.T) {
	empty := imageAdapter("stub", provider.Outcome{Image: &provider.ImageResult{}, UsageSource: contract.UsageProviderReported}, nil)
	backup := imageAdapter("backup", producedImages(1), nil)
	_, result := harness{deployments: twoDeploymentsOfOneRevision, adapters: []provider.Adapter{empty, backup}, retry: &noRetries}.run(t, imageRequest())
	if result.Failure != nil || result.Image == nil || len(result.Image.Images) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if empty.attempts() != 1 || backup.attempts() != 1 {
		t.Errorf("attempts = %d then %d", empty.attempts(), backup.attempts())
	}
	if result.Report.DeploymentID != "dep_b" || result.Report.RouteSwitches != 1 {
		t.Errorf("report = %+v", result.Report)
	}
	attempts := result.UpstreamCost.Attempts
	if len(attempts) != 2 || attempts[0].Served || !attempts[1].Served {
		t.Errorf("cost attempts = %+v; want the empty one unserved, then the serving one", attempts)
	}
}

// A provider fault moves on exactly as it does for any family; a refusal of
// the request itself does not.
func TestImageFailoverSemanticsAreTheOrdinaryOnes(t *testing.T) {
	down := imageAdapter("stub", provider.Outcome{}, overloaded("stub"))
	backup := imageAdapter("backup", producedImages(1), nil)
	_, result := harness{deployments: twoDeploymentsOfOneRevision, adapters: []provider.Adapter{down, backup}, retry: &noRetries}.run(t, imageRequest())
	if result.Failure != nil || result.Image == nil {
		t.Fatalf("an overloaded deployment was not failed over: %+v", result.Failure)
	}

	refused := imageAdapter("stub", provider.Outcome{}, customerFault("stub"))
	untouched := imageAdapter("backup", producedImages(1), nil)
	_, result = harness{deployments: twoDeploymentsOfOneRevision, adapters: []provider.Adapter{refused, untouched}, retry: &noRetries}.run(t, imageRequest())
	if result.Failure == nil || result.Image != nil || untouched.attempts() != 0 {
		t.Errorf("a refused request moved on: failure=%+v, second attempts=%d", result.Failure, untouched.attempts())
	}
}

func withSingleRoute(request *contract.Request) *contract.Request {
	request.AuthorizedRoutes = request.AuthorizedRoutes[:1]
	return request
}
