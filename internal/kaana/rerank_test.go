package kaana_test

import (
	"context"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func rerankRequest() *contract.Request {
	request := authorizedRequest()
	query := "a synthetic query"
	request.Modality = contract.ModalityText
	request.Input = contract.Input{Format: contract.InputText, Text: &query}
	request.Rerank = &contract.RerankParameters{Documents: []string{"first", "second"}}
	request.Stream = false
	request.Client.APIFormat = contract.APIFormatRerank
	request.Client.Endpoint = "/v1/rerank"
	return request
}

func rerankAdapter(slug contract.ProviderSlug, outcome provider.Outcome, failure error) *scriptedAdapter {
	return &scriptedAdapter{slug: slug, stream: func(context.Context, *provider.Call, provider.Emitter) (provider.Outcome, error) {
		return outcome, failure
	}}
}

func producedRanking(documents int) provider.Outcome {
	ranked := make([]provider.RankedDocument, documents)
	for index := range ranked {
		ranked[index] = provider.RankedDocument{Index: index, RelevanceScore: 1 / float64(index+1)}
	}
	return provider.Outcome{
		Rerank:      &provider.RerankResult{Results: ranked, InputTokens: 30},
		Units:       []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 30}},
		UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop,
	}
}

func TestARerankRequestIsServedWithItsRankingAndUnits(t *testing.T) {
	primary := rerankAdapter("stub", producedRanking(2), nil)
	_, result := harness{deployments: twoDeploymentsOfOneRevision, adapters: []provider.Adapter{primary, succeedingAdapter("backup", 1)}, retry: &noRetries}.run(t, rerankRequest())
	if result.Failure != nil || result.Rerank == nil || len(result.Rerank.Results) != 2 {
		t.Fatalf("result = %+v", result)
	}
	report := result.Report
	if report == nil || report.Outcome != contract.OutcomeCompleted || report.UsageSource != contract.UsageProviderReported || report.DeploymentID != "dep_a" {
		t.Fatalf("report = %+v", report)
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(report.Units) != 2 || report.Units[0].Unit != contract.UnitRequests || report.Units[1].Unit != contract.UnitInputTokens || report.Units[1].Quantity != 30 {
		t.Errorf("units = %+v", report.Units)
	}
	attempts := result.UpstreamCost.Attempts
	if len(attempts) != 1 || !attempts[0].Served {
		t.Errorf("cost attempts = %+v; want one served row", attempts)
	}
}

// An adapter that reports success without scoring a document has produced
// nothing. The attempt is the deployment's failure, not an empty ranking served
// and billed (#266): the customer is told provider_error, reported no units,
// and the attempt is an unserved cost row.
func TestARerankAttemptThatScoredNothingIsAFailureNotAnAnswer(t *testing.T) {
	for name, outcome := range map[string]provider.Outcome{
		"no rerank member": {Units: []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}, UsageSource: contract.UsageProviderReported},
		"an empty ranking": {Rerank: &provider.RerankResult{}, Units: []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 12}}, UsageSource: contract.UsageProviderReported},
	} {
		t.Run(name, func(t *testing.T) {
			adapter := rerankAdapter("stub", outcome, nil)
			_, result := harness{deployments: twoDeploymentsOfOneRevision, adapters: []provider.Adapter{adapter, succeedingAdapter("backup", 1)}, retry: &noRetries}.run(t, withSingleRoute(rerankRequest()))
			if result.Rerank != nil {
				t.Fatalf("an attempt that scored nothing was served: %+v", result.Rerank)
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
func TestAnEmptyRerankAttemptFailsOverToTheNextRoute(t *testing.T) {
	empty := rerankAdapter("stub", provider.Outcome{Rerank: &provider.RerankResult{}, UsageSource: contract.UsageProviderReported}, nil)
	backup := rerankAdapter("backup", producedRanking(2), nil)
	_, result := harness{deployments: twoDeploymentsOfOneRevision, adapters: []provider.Adapter{empty, backup}, retry: &noRetries}.run(t, rerankRequest())
	if result.Failure != nil || result.Rerank == nil || len(result.Rerank.Results) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if empty.attempts() != 1 || backup.attempts() != 1 {
		t.Errorf("attempts = %d then %d", empty.attempts(), backup.attempts())
	}
	attempts := result.UpstreamCost.Attempts
	if len(attempts) != 2 || attempts[0].Served || !attempts[1].Served {
		t.Errorf("cost attempts = %+v; want the empty one unserved, then the serving one", attempts)
	}
}
