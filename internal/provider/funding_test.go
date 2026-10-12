package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// scriptedSender answers each key with the failure scripted for it, or a 200.
type scriptedSender struct {
	failures map[string]error
	sent     []string
}

func (s *scriptedSender) Send(_ context.Context, _ *Call, key Key) (*http.Response, error) {
	s.sent = append(s.sent, key.ID)
	status := http.StatusOK
	if s.failures[key.ID] != nil {
		status = http.StatusBadGateway
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: http.NoBody, Request: &http.Request{Header: http.Header{"X-Key": {key.ID}}}}, nil
}

func (s *scriptedSender) Refuse(response *http.Response, _ Key) error {
	_ = response.Body.Close()
	return s.failures[response.Request.Header.Get("X-Key")]
}

func (s *scriptedSender) TransportFailure(_ context.Context, err error) error { return err }

func fundedPool(t *testing.T, declarations ...KeyDeclaration) *KeyPool {
	t.Helper()
	for index := range declarations {
		declarations[index].Secret = "kaana-unit-fake-funding-" + declarations[index].KeyID
	}
	pool, err := NewKeyPool("anthropic", declarations, KeyPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func walkOnce(t *testing.T, pool *KeyPool, sender *scriptedSender, sequence *CredentialAttemptSequence) (Key, error) {
	t.Helper()
	call := &Call{RequestID: "req-funding", Route: Route{DeploymentID: "dep_anthropic_test", Provider: "anthropic"}, CredentialAttempts: sequence}
	response, key, err := Walk(context.Background(), pool, call, sender)
	if response != nil {
		_ = response.Body.Close()
	}
	return key, err
}

func TestFundingTakesTheDearerOfTwoStatements(t *testing.T) {
	for _, testCase := range []struct {
		class    KeyClass
		category string
		want     Funding
	}{
		{KeyClassUnstated, "", FundingUnstated},
		{KeyClassFree, "", FundingFree},
		{KeyClassPaid, "", FundingPaid},
		{KeyClassUnstated, "promotional", FundingPromotional},
		{KeyClassUnstated, "trial", FundingPromotional},
		{KeyClassUnstated, "prepaid", FundingPrepaid},
		{KeyClassFree, "promotional", FundingPromotional},
		{KeyClassPaid, "promotional", FundingPaid},
		{KeyClassFree, "paid", FundingPaid},
	} {
		got, err := FundingFor(testCase.class, testCase.category)
		if err != nil || got != testCase.want {
			t.Errorf("FundingFor(%q, %q) = %q, %v; want %q", testCase.class, testCase.category, got, err, testCase.want)
		}
	}
	if _, err := FundingFor(KeyClassUnstated, "gift"); err == nil {
		t.Error("an unknown capacity category was accepted")
	}
}

// The owner's order: free, then credits (a grant before a prepaid balance),
// then paid; unstated last; within one funding, the declared order.
func TestAPoolIsWalkedFreeThenCreditsThenPaid(t *testing.T) {
	pool := fundedPool(t,
		KeyDeclaration{KeyID: "unstated"},
		KeyDeclaration{KeyID: "paid", Funding: FundingPaid},
		KeyDeclaration{KeyID: "prepaid", Funding: FundingPrepaid},
		KeyDeclaration{KeyID: "promo-a", Funding: FundingPromotional},
		KeyDeclaration{KeyID: "free", Class: KeyClassFree},
		KeyDeclaration{KeyID: "promo-b", Funding: FundingPromotional},
	)
	got := strings.Join(poolOrder(t, pool), ",")
	if want := "free,promo-a,promo-b,prepaid,paid,unstated"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

// Anthropic's billing_error on key A (spent grant) moves the same request to
// key B; once A's retirement has expired, the next request goes back to A
// (cheapest first) and its success proves the recovery.
func TestASpentKeyMovesTheRequestOnAndComesBackWhenItRecovers(t *testing.T) {
	pool := fundedPool(t,
		KeyDeclaration{KeyID: "key-a", Funding: FundingPromotional},
		KeyDeclaration{KeyID: "key-b", Funding: FundingPromotional},
	)
	spent := &scriptedSender{failures: map[string]error{
		"key-a": upstreamFailure(contract.CodeProviderBillingRefused, contract.UpstreamQuota),
	}}
	key, err := walkOnce(t, pool, spent, nil)
	if err != nil || key.ID != "key-b" {
		t.Fatalf("served by %q, %v; want key-b after key-a's billing refusal", key.ID, err)
	}
	if strings.Join(spent.sent, ",") != "key-a,key-b" {
		t.Fatalf("sent %v", spent.sent)
	}
	// While A is retired, a new request goes straight to B.
	again := &scriptedSender{}
	if key, err := walkOnce(t, pool, again, nil); err != nil || key.ID != "key-b" || len(again.sent) != 1 {
		t.Fatalf("while key-a is retired: served by %q (%v), sent %v", key.ID, err, again.sent)
	}
	// A's retirement expires (the provider's reset time has passed).
	past := time.Now().Add(-time.Hour)
	pool.Retire(Key{Position: 1, ID: "key-a"}, KeyExhausted, past, past.Add(time.Minute))
	recovered := &scriptedSender{}
	key, err = walkOnce(t, pool, recovered, nil)
	if err != nil || key.ID != "key-a" {
		t.Fatalf("after recovery served by %q, %v; want key-a, the cheapest", key.ID, err)
	}
	if health := pool.Projection(time.Now()); health.Usable != 2 {
		t.Fatalf("a successful request did not prove recovery: %+v", health)
	}
}

// A transient failure retires nothing, but the same request's retry starts on
// the next key; a new request starts on the cheapest key again.
func TestATransientFailureMovesTheRetryToTheNextKey(t *testing.T) {
	pool := fundedPool(t,
		KeyDeclaration{KeyID: "key-a", Funding: FundingPromotional},
		KeyDeclaration{KeyID: "key-b", Funding: FundingPaid},
	)
	down := &scriptedSender{failures: map[string]error{
		"key-a": upstreamFailure(contract.CodeProviderOverloaded, contract.UpstreamServerError),
	}}
	sequence := &CredentialAttemptSequence{}
	if key, err := walkOnce(t, pool, down, sequence); err == nil || key.ID != "key-a" {
		t.Fatalf("first walk: %q, %v; want key-a's own failure", key.ID, err)
	}
	if key, err := walkOnce(t, pool, down, sequence); err != nil || key.ID != "key-b" {
		t.Fatalf("retry: %q, %v; want key-b", key.ID, err)
	}
	if health := pool.Projection(time.Now()); health.Usable != 2 {
		t.Fatalf("a transient failure retired a key: %+v", health)
	}
	fresh := &scriptedSender{}
	if key, _ := walkOnce(t, pool, fresh, &CredentialAttemptSequence{}); key.ID != "key-a" {
		t.Fatalf("a new request started on %q, want key-a", key.ID)
	}
}

// With only one key the retry reuses it: a throttle is not exhaustion.
func TestATransientFailureOnTheOnlyKeyRetriesThatKey(t *testing.T) {
	pool := fundedPool(t, KeyDeclaration{KeyID: "only", Funding: FundingPaid})
	down := &scriptedSender{failures: map[string]error{"only": upstreamFailure(contract.CodeProviderError, contract.UpstreamServerError)}}
	sequence := &CredentialAttemptSequence{}
	_, _ = walkOnce(t, pool, down, sequence)
	if key, _ := walkOnce(t, pool, down, sequence); key.ID != "only" {
		t.Fatalf("retry leased %q", key.ID)
	}
}

// A request fault is retried nowhere: the next key would be refused alike.
func TestARequestFaultNeverMovesToAnotherKey(t *testing.T) {
	pool := fundedPool(t, KeyDeclaration{KeyID: "key-a", Funding: FundingPromotional}, KeyDeclaration{KeyID: "key-b", Funding: FundingPromotional})
	fault := &scriptedSender{failures: map[string]error{"key-a": upstreamFailure(contract.CodeInvalidRequest, contract.UpstreamInvalidReq)}}
	if _, err := walkOnce(t, pool, fault, nil); err == nil || len(fault.sent) != 1 {
		t.Fatalf("a request fault moved: sent %v, %v", fault.sent, err)
	}
}

// Oxy authorized the route on credit funding only: a spent grant never falls
// through to a paid key or to a key whose cost nobody stated.
func TestAFundingCeilingNeverSpendsADearerKey(t *testing.T) {
	pool := fundedPool(t,
		KeyDeclaration{KeyID: "promo", Funding: FundingPromotional},
		KeyDeclaration{KeyID: "paid", Funding: FundingPaid},
		KeyDeclaration{KeyID: "unstated"},
	)
	credit, err := pool.WithinFunding(FundingTierCredit)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(poolOrder(t, credit), ","); got != "promo" {
		t.Fatalf("a credit-only view walks %s", got)
	}
	spent := &scriptedSender{failures: map[string]error{"promo": upstreamFailure(contract.CodeProviderBillingRefused, contract.UpstreamQuota)}}
	_, err = walkOnce(t, credit, spent, nil)
	var upstream ErrUpstream
	if !errors.As(err, &upstream) || upstream.Code != contract.CodeProviderBillingRefused || strings.Join(spent.sent, ",") != "promo" {
		t.Fatalf("a spent grant under a credit ceiling: sent %v, %v", spent.sent, err)
	}
	// Once the grant is retired, the view has nothing to lease and says when.
	_, err = walkOnce(t, credit, &scriptedSender{}, nil)
	if !errors.As(err, &upstream) || upstream.Code != contract.CodeDeploymentUnavailable {
		t.Fatalf("an exhausted credit view: %v", err)
	}
	// The unbounded pool still serves on the paid key.
	if key, err := walkOnce(t, pool, &scriptedSender{}, nil); err != nil || key.ID != "paid" {
		t.Fatalf("the unbounded pool served %q, %v", key.ID, err)
	}
	// No key within the tier is a route this pool cannot serve.
	if _, err := pool.WithinFunding(FundingTierFree); err == nil {
		t.Fatal("a free ceiling was granted on a pool with no free key")
	}
	if _, err := pool.WithinFunding("cheap"); err == nil {
		t.Fatal("an unknown tier was accepted")
	}
	// A pin and a ceiling compose: the pin must itself be admitted.
	pinned, _ := pool.Bind("paid")
	if _, err := pinned.WithinFunding(FundingTierCredit); err == nil {
		t.Fatal("a paid pin was admitted under a credit ceiling")
	}
}
