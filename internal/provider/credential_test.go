package provider

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
)

const (
	// The credentials below are test strings and nothing else. They avoid every
	// real provider's key prefix so a secret scanner has nothing to flag, and no
	// real provider key appears in this repository, in a test, or in CI.
	firstCredential  = "relay-unit-fake-credential-0000"
	secondCredential = "relay-unit-fake-credential-0001"
	thirdCredential  = "relay-unit-fake-credential-0002"
)

func upstreamFailure(code contract.ErrorCode, category contract.UpstreamErrorCategory) error {
	return ErrUpstream{Code: code, Category: category, Detail: "upstream said so"}
}

/* -------------------------------------------------------------------------- */
/*  The classifier                                                            */
/* -------------------------------------------------------------------------- */

// TestAFailureSaysThreeDifferentThingsAndTheyDoNotAgree is the classifier's
// table, and it is the load-bearing test in this package.
//
// One upstream failure answers three questions at once — what the CUSTOMER is
// told, whether the DEPLOYMENT is to blame, and whether the CREDENTIAL is — and
// the answers routinely differ. A refused credential is non-retryable for the
// customer, attributable to the deployment, and terminal for the key; an
// exhausted quota is non-retryable, attributable, and the one case that moves
// the request to another key; a customer's own exceeded quota is none of those
// things about the key at all.
//
// Collapsing any two rows below is exactly how a build ends up retiring working
// credentials, so the table is asserted per code rather than per family.
func TestAFailureSaysThreeDifferentThingsAndTheyDoNotAgree(t *testing.T) {
	cases := map[string]struct {
		failure error
		verdict CredentialVerdict
	}{
		// Terminal for the key.
		"the provider refused this credential": {
			failure: upstreamFailure(contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication),
			verdict: CredentialRejected,
		},
		"the account behind this credential cannot be billed": {
			failure: upstreamFailure(contract.CodeProviderBillingRefused, contract.UpstreamQuota),
			verdict: CredentialExhausted,
		},

		// Transient. The key is untouched; whether the REQUEST goes anywhere is
		// the deployment lane's decision, not this one's.
		"a burst rate limit": {
			failure: upstreamFailure(contract.CodeRateLimited, contract.UpstreamRateLimit),
			verdict: CredentialHealthy,
		},
		"the provider timed out": {
			failure: upstreamFailure(contract.CodeProviderTimeout, contract.UpstreamTimeout),
			verdict: CredentialHealthy,
		},
		"the provider is overloaded": {
			failure: upstreamFailure(contract.CodeProviderOverloaded, contract.UpstreamOverloaded),
			verdict: CredentialHealthy,
		},
		"the provider returned an internal error": {
			failure: upstreamFailure(contract.CodeProviderError, contract.UpstreamServerError),
			verdict: CredentialHealthy,
		},

		// Terminal for the request. Every other credential would be refused
		// identically, so retrying one would multiply a customer's mistake into
		// a call per key.
		"the request is invalid": {
			failure: upstreamFailure(contract.CodeInvalidRequest, contract.UpstreamInvalidReq),
			verdict: CredentialRequestFault,
		},
		"the provider does not serve this model": {
			failure: upstreamFailure(contract.CodeModelNotFound, contract.UpstreamInvalidReq),
			verdict: CredentialRequestFault,
		},
		"the modality is not supported": {
			failure: upstreamFailure(contract.CodeUnsupportedModality, contract.UpstreamInvalidReq),
			verdict: CredentialRequestFault,
		},
		"the request is too large": {
			failure: upstreamFailure(contract.CodeRequestTooLarge, contract.UpstreamInvalidReq),
			verdict: CredentialRequestFault,
		},
		"a content filter stopped it": {
			failure: upstreamFailure(contract.CodeUpstreamContentFiltered, contract.UpstreamContentFilter),
			verdict: CredentialRequestFault,
		},
		"permission was denied": {
			failure: upstreamFailure(contract.CodePermissionDenied, contract.UpstreamInvalidReq),
			verdict: CredentialRequestFault,
		},

		// The customer's own money, which says nothing about Relay's key. This
		// is the mirror image of the mistake the whole file is about: retiring
		// a working credential because a CUSTOMER ran out.
		"the customer's quota is exhausted": {
			failure: upstreamFailure(contract.CodeQuotaExceeded, contract.UpstreamQuota),
			verdict: CredentialRequestFault,
		},
		"the customer's balance is insufficient": {
			failure: upstreamFailure(contract.CodeInsufficientBalance, contract.UpstreamQuota),
			verdict: CredentialRequestFault,
		},
		"the customer's spending limit was reached": {
			failure: upstreamFailure(contract.CodeSpendingLimitExceeded, contract.UpstreamQuota),
			verdict: CredentialRequestFault,
		},
	}

	seen := make(map[CredentialVerdict]int, 4)
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := CredentialVerdictFor(testCase.failure); got != testCase.verdict {
				t.Errorf("classified as %q, expected %q", got, testCase.verdict)
			}
		})
		seen[testCase.verdict]++
	}

	// The vacuity floor: a classifier that answered one thing for everything
	// would satisfy any subset of this table that happened to agree with it.
	for _, verdict := range []CredentialVerdict{CredentialHealthy, CredentialExhausted, CredentialRejected, CredentialRequestFault} {
		if seen[verdict] == 0 {
			t.Errorf("the table exercises no case of %q, so a classifier that never returns it would pass", verdict)
		}
	}
}

// TestAStateNobodyCouldDetermineIsNotExhaustion is the rule the whole design
// rests on, stated as a test.
//
// Every input here is a failure nothing classified — a wrapped sentinel, an
// error with no upstream shape, an error carrying a code this build does not
// know. Answering "exhausted" to any of them disables a working credential on
// an ambiguous signal, which is worse than the problem a pool solves.
func TestAStateNobodyCouldDetermineIsNotExhaustion(t *testing.T) {
	unclassified := []struct {
		name    string
		failure error
	}{
		{name: "a bare error", failure: errors.New("something went wrong")},
		{name: "a wrapped error", failure: fmt.Errorf("reading the upstream response: %w", errors.New("connection reset"))},
		{name: "an upstream failure carrying a code this build does not know", failure: ErrUpstream{Code: "a_code_from_a_later_contract", Category: contract.UpstreamUnknown}},
		{name: "an upstream failure carrying no code at all", failure: ErrUpstream{Category: contract.UpstreamUnknown}},
	}

	for _, testCase := range unclassified {
		t.Run(testCase.name, func(t *testing.T) {
			if got := CredentialVerdictFor(testCase.failure); got != CredentialHealthy {
				t.Fatalf("an unclassifiable failure was read as %q; unknown is not exhausted", got)
			}

			// And the pool must act on that: the key is still there afterwards.
			pool := newTestPool(t, KeyPolicy{}, firstCredential, secondCredential)
			key, _ := pool.Begin().Next(time.Now())
			if verdict := CredentialVerdictFor(testCase.failure); verdict == CredentialExhausted || verdict == CredentialRejected {
				pool.Retire(key, KeyExhausted, time.Now(), time.Time{})
			}
			if usable := pool.Projection(time.Now()).Usable; usable != 2 {
				t.Errorf("%d of 2 credentials are usable after an unclassifiable failure", usable)
			}
		})
	}
}

// TestAThrottleIsNeverConfusedWithExhaustion pins the pair that shares a status
// at every OpenAI-compatible provider.
func TestAThrottleIsNeverConfusedWithExhaustion(t *testing.T) {
	throttle := upstreamFailure(contract.CodeRateLimited, contract.UpstreamRateLimit)
	exhausted := upstreamFailure(contract.CodeProviderBillingRefused, contract.UpstreamQuota)

	if !Throttled(throttle) {
		t.Error("a rate limit is not recognised as a throttle")
	}
	if Throttled(exhausted) {
		t.Error("an exhausted account is recognised as a throttle, which would leave a spent key in rotation forever")
	}
	if Throttled(errors.New("unclassified")) {
		t.Error("an unclassified failure is recognised as a throttle")
	}
}

/* -------------------------------------------------------------------------- */
/*  The pool                                                                  */
/* -------------------------------------------------------------------------- */

func newTestPool(t *testing.T, policy KeyPolicy, secrets ...string) *KeyPool {
	t.Helper()
	pool, err := NewKeyPool("test-provider", secrets, policy, nil)
	if err != nil {
		t.Fatalf("building the pool: %v", err)
	}
	return pool
}

// TestARetiredKeyIsServedByTheNextOne is the pool's reason for existing.
func TestARetiredKeyIsServedByTheNextOne(t *testing.T) {
	now := time.Now()
	pool := newTestPool(t, KeyPolicy{}, firstCredential, secondCredential)

	first, leased := pool.Begin().Next(now)
	if !leased || first.Position != 1 {
		t.Fatalf("the first lease returned position %d (leased: %v); keys are spent in the order they were declared", first.Position, leased)
	}
	pool.Retire(first, KeyExhausted, now, time.Time{})

	next, leased := pool.Begin().Next(now)
	if !leased {
		t.Fatal("a pool with one spent key and one untouched key leased nothing")
	}
	if next.Position != 2 {
		t.Errorf("the second lease returned position %d, expected 2", next.Position)
	}
	if next.Secret() != secondCredential {
		t.Error("the second lease returned the wrong credential")
	}
}

// TestARetirementExpires is why a spent key is not a dead one.
//
// A quota resets on the provider's own cycle and a revoked key comes back when
// an operator rotates it. A process that retired a credential permanently would
// serve nothing for that provider until somebody restarted it, and could not be
// told otherwise.
func TestARetirementExpires(t *testing.T) {
	now := time.Now()
	pool := newTestPool(t, KeyPolicy{Retirement: time.Minute}, firstCredential)

	key, _ := pool.Begin().Next(now)
	pool.Retire(key, KeyExhausted, now, time.Time{})

	if _, leased := pool.Begin().Next(now.Add(59 * time.Second)); leased {
		t.Error("a key retired for a minute was leased 59 seconds later")
	}
	if _, leased := pool.Begin().Next(now.Add(61 * time.Second)); !leased {
		t.Error("a key retired for a minute was still out 61 seconds later, so nothing ever returns to rotation")
	}
}

// TestTheProvidersOwnResetTimeIsBelievedOverThePolicysWindow.
//
// The window is a guess about when capacity returns; a provider that said when
// it returns is not guessing.
func TestTheProvidersOwnResetTimeIsBelievedOverThePolicysWindow(t *testing.T) {
	now := time.Now()
	pool := newTestPool(t, KeyPolicy{Retirement: time.Hour}, firstCredential)

	key, _ := pool.Begin().Next(now)
	pool.Retire(key, KeyExhausted, now, now.Add(time.Minute))

	if _, leased := pool.Begin().Next(now.Add(2 * time.Minute)); !leased {
		t.Error("the provider said the capacity returns in a minute and the key was still out after two")
	}
}

// TestNoKeyIsLeasedTwiceForOneRequest is what bounds key failover without a
// configured ceiling: a request can make at most as many upstream calls as the
// pool has keys, and the bound is a property of the walk.
func TestNoKeyIsLeasedTwiceForOneRequest(t *testing.T) {
	now := time.Now()
	pool := newTestPool(t, KeyPolicy{}, firstCredential, secondCredential, thirdCredential)
	attempt := pool.Begin()

	seen := make(map[int]bool)
	for range 3 {
		key, leased := attempt.Next(now)
		if !leased {
			t.Fatalf("the walk ended after %d of 3 keys", len(seen))
		}
		if seen[key.Position] {
			t.Fatalf("position %d was leased twice for one request", key.Position)
		}
		seen[key.Position] = true
	}
	if _, leased := attempt.Next(now); leased {
		t.Error("a fourth lease was granted from a pool of three keys")
	}

	// A separate request starts again from the first key: the walk is
	// request-scoped, not a cursor the pool carries between them.
	if key, leased := pool.Begin().Next(now); !leased || key.Position != 1 {
		t.Errorf("a new request leased position %d (leased: %v), expected to start again at 1", key.Position, leased)
	}
}

// TestAThrottleRotatesOnlyWhereTheOperatorSaidTheKeysAreSeparateAccounts.
//
// Keys of one account share that account's rate limit, so rotating into it
// hammers a provider that has just asked for less traffic. Keys of separate
// accounts have separate limits. Relay cannot tell which it holds, so it does
// not guess.
func TestAThrottleRotatesOnlyWhereTheOperatorSaidTheKeysAreSeparateAccounts(t *testing.T) {
	shared := newTestPool(t, KeyPolicy{}, firstCredential, secondCredential).Begin()
	if shared.AllowThrottleRotation() {
		t.Error("a throttle rotated onto another key of the same account, which walks into the limit that was just reached")
	}

	separate := newTestPool(t, KeyPolicy{OnSeparateAccounts: true}, firstCredential, secondCredential).Begin()
	if !separate.AllowThrottleRotation() {
		t.Fatal("the operator declared separate accounts and a throttle did not rotate")
	}
	if separate.AllowThrottleRotation() {
		t.Error("a second throttle rotation was allowed in one request; nothing is retired on a throttle, so this would repeat in full on every request for as long as the throttle lasted")
	}
}

// TestAPoolRefusesWhatWouldPresentAsWorkingAndBehaveAsBroken.
func TestAPoolRefusesWhatWouldPresentAsWorkingAndBehaveAsBroken(t *testing.T) {
	if _, err := NewKeyPool("test-provider", []string{firstCredential, "   "}, KeyPolicy{}, nil); err == nil {
		t.Error("a blank credential was accepted; it would be sent and refused, and the pool would report a key it does not have")
	}

	_, err := NewKeyPool("test-provider", []string{firstCredential, firstCredential}, KeyPolicy{}, nil)
	if err == nil {
		t.Fatal("the same credential twice was accepted; it looks like two keys, exhausts as one, and halves the pool the first time it is spent")
	}
	// The error names positions. A message that quoted the credential would put
	// a secret in a startup log, which is where they are read from.
	if strings.Contains(err.Error(), firstCredential) {
		t.Errorf("the refusal quotes the credential: %q", err)
	}

	// The control: two different credentials are accepted, or the two checks
	// above would pass on a constructor that refuses everything.
	if _, err := NewKeyPool("test-provider", []string{firstCredential, secondCredential}, KeyPolicy{}, nil); err != nil {
		t.Errorf("two distinct credentials were refused: %v", err)
	}
}

// TestAnOperatorGapIsNotASpentPool.
//
// Both states leave a request unservable and they are different problems: one
// is a credential nobody configured, which no retry creates, and the other is a
// pool that will serve again at a moment this can name.
func TestAnOperatorGapIsNotASpentPool(t *testing.T) {
	now := time.Now()

	empty := newTestPool(t, KeyPolicy{})
	var unconfigured ErrUpstream
	if !errors.As(empty.NoUsableCredential(now), &unconfigured) {
		t.Fatal("an unconfigured pool produced an unclassified failure")
	}
	if unconfigured.Code != contract.CodeProviderCredentialInvalid {
		t.Errorf("an unconfigured provider reports %q", unconfigured.Code)
	}
	if unconfigured.Code.Retryable() {
		t.Error("an unconfigured provider was reported retryable; no retry creates a credential")
	}
	if !AttributableCategory(unconfigured.Category) {
		t.Error("an unconfigured provider is not attributable, so the deployment stays in rotation failing every request sent to it")
	}

	spent := newTestPool(t, KeyPolicy{Retirement: time.Minute}, firstCredential)
	key, _ := spent.Begin().Next(now)
	spent.Retire(key, KeyExhausted, now, time.Time{})

	var unavailable ErrUpstream
	if !errors.As(spent.NoUsableCredential(now), &unavailable) {
		t.Fatal("a spent pool produced an unclassified failure")
	}
	if unavailable.Code != contract.CodeDeploymentUnavailable {
		t.Errorf("a spent pool reports %q", unavailable.Code)
	}
	if !unavailable.Code.Retryable() {
		t.Error("a spent pool was reported non-retryable, though it serves again when the retirement expires")
	}
	// The hint is the moment the earliest key returns, not a number chosen to
	// look reasonable.
	if unavailable.RetryAfterMs < 59_000 || unavailable.RetryAfterMs > 60_000 {
		t.Errorf("the retry hint is %dms; the earliest key returns in a minute", unavailable.RetryAfterMs)
	}
	if strings.Contains(unavailable.Detail, firstCredential) {
		t.Error("the failure quotes a credential")
	}
}

/* -------------------------------------------------------------------------- */
/*  Quota telemetry                                                           */
/* -------------------------------------------------------------------------- */

// TestOnlyADeclaredHeaderReportingZeroIsExhaustion.
//
// Every row that is not `exhausted` is a row where a build that read header
// names for their meaning would have retired a healthy key.
func TestOnlyADeclaredHeaderReportingZeroIsExhaustion(t *testing.T) {
	declared := QuotaHeaders{
		"x-fake-credits-remaining": SignalRemainingCredits,
		"x-fake-credits-reset":     SignalResetAt,
	}

	cases := map[string]struct {
		mapping QuotaHeaders
		header  http.Header
		state   QuotaState
	}{
		"a declared header reporting zero": {
			mapping: declared,
			header:  http.Header{"X-Fake-Credits-Remaining": []string{"0"}},
			state:   QuotaExhausted,
		},
		"a declared header reporting capacity": {
			mapping: declared,
			header:  http.Header{"X-Fake-Credits-Remaining": []string{"1200"}},
			state:   QuotaHealthy,
		},
		"a declared header carrying something unreadable": {
			mapping: declared,
			header:  http.Header{"X-Fake-Credits-Remaining": []string{"plenty"}},
			state:   QuotaUnavailable,
		},
		"a declared header that is absent": {
			mapping: declared,
			header:  http.Header{"Content-Type": []string{"application/json"}},
			state:   QuotaUnknown,
		},
		// The rule that keeps a throttle from reading as exhaustion. This is a
		// header several providers really send, it really does reach zero on a
		// burst limit, and no mapping declares what it means — so it means
		// nothing.
		"a rate-limit header nobody declared": {
			mapping: declared,
			header:  http.Header{"X-Ratelimit-Remaining": []string{"0"}},
			state:   QuotaUnknown,
		},
		"a provider with no mapping at all": {
			mapping: nil,
			header:  http.Header{"X-Fake-Credits-Remaining": []string{"0"}},
			state:   QuotaUnknown,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := testCase.mapping.Read(testCase.header).State; got != testCase.state {
				t.Errorf("read as %q, expected %q", got, testCase.state)
			}
		})
	}
}

// TestOnlyExhaustionTakesAKeyOutOfRotation is the same rule at the pool.
//
// Read() answering correctly is half of it; the other half is that nothing but
// `exhausted` acts on the answer, which is where a build that treated
// `unavailable` as "assume the worst" would fail.
func TestOnlyExhaustionTakesAKeyOutOfRotation(t *testing.T) {
	mapping := QuotaHeaders{"x-fake-credits-remaining": SignalRemainingCredits}

	for name, testCase := range map[string]struct {
		header http.Header
		usable int
	}{
		"zero remaining":      {header: http.Header{"X-Fake-Credits-Remaining": []string{"0"}}, usable: 1},
		"capacity remaining":  {header: http.Header{"X-Fake-Credits-Remaining": []string{"5"}}, usable: 2},
		"an unreadable value": {header: http.Header{"X-Fake-Credits-Remaining": []string{"lots"}}, usable: 2},
		"no such header":      {header: http.Header{}, usable: 2},
		"an undeclared header": {
			header: http.Header{"X-Ratelimit-Remaining": []string{"0"}}, usable: 2,
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			pool, err := NewKeyPool("test-provider", []string{firstCredential, secondCredential}, KeyPolicy{}, mapping)
			if err != nil {
				t.Fatalf("building the pool: %v", err)
			}
			key, _ := pool.Begin().Next(now)
			pool.Observe(key, testCase.header, now)

			if got := pool.Projection(now).Usable; got != testCase.usable {
				t.Errorf("%d of 2 credentials are usable, expected %d", got, testCase.usable)
			}
		})
	}
}

// TestAResetHeaderSetsTheRetirementAndCanNeverCauseOne.
func TestAResetHeaderSetsTheRetirementAndCanNeverCauseOne(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	mapping := QuotaHeaders{
		"x-fake-credits-remaining": SignalRemainingCredits,
		"x-fake-credits-reset":     SignalResetAt,
	}
	pool, err := NewKeyPool("test-provider", []string{firstCredential, secondCredential}, KeyPolicy{Retirement: time.Hour}, mapping)
	if err != nil {
		t.Fatalf("building the pool: %v", err)
	}

	// A reset time on its own retires nothing: it says when capacity returns,
	// never that there is none.
	key, _ := pool.Begin().Next(now)
	pool.Observe(key, http.Header{"X-Fake-Credits-Reset": []string{fmt.Sprint(now.Add(time.Minute).Unix())}}, now)
	if usable := pool.Projection(now).Usable; usable != 2 {
		t.Fatalf("a reset time alone retired a key: %d of 2 usable", usable)
	}

	pool.Observe(key, http.Header{
		"X-Fake-Credits-Remaining": []string{"0"},
		"X-Fake-Credits-Reset":     []string{fmt.Sprint(now.Add(time.Minute).Unix())},
	}, now)
	if _, leased := pool.Begin().Next(now.Add(2 * time.Minute)); !leased {
		t.Error("the provider said the capacity returns in a minute; the key was still out after two, so the policy's hour overrode the provider's own answer")
	}
}

/* -------------------------------------------------------------------------- */
/*  The projection                                                            */
/* -------------------------------------------------------------------------- */

// TestTheProjectionCarriesNoCredential.
//
// Not even a truncated hash of one: a fingerprint of a secret confirms a
// guessed secret, and a position is enough to name a key.
func TestTheProjectionCarriesNoCredential(t *testing.T) {
	now := time.Now()
	pool := newTestPool(t, KeyPolicy{Retirement: time.Minute}, firstCredential, secondCredential)

	key, _ := pool.Begin().Next(now)
	pool.Retire(key, KeyRejected, now, time.Time{})

	projection := pool.Projection(now)
	if projection.Declared != 2 || projection.Usable != 1 {
		t.Errorf("the projection reports %d declared and %d usable, expected 2 and 1", projection.Declared, projection.Usable)
	}
	if len(projection.Keys) != 2 {
		t.Fatalf("the projection carries %d keys, expected 2", len(projection.Keys))
	}
	if projection.Keys[0].State != string(KeyRejected) {
		t.Errorf("the retired key reports state %q, expected %q", projection.Keys[0].State, KeyRejected)
	}
	if projection.Keys[0].RetiredUntil == nil {
		t.Error("a retired key does not say when it returns")
	}
	if projection.Keys[1].State != "usable" || projection.Keys[1].RetiredUntil != nil {
		t.Errorf("the untouched key reports %q", projection.Keys[1].State)
	}

	rendered := fmt.Sprintf("%+v", projection)
	for _, secret := range []string{firstCredential, secondCredential} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("the projection carries a credential: %s", rendered)
		}
	}
}
