package publicapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

type replayControl struct {
	publicapi.NativeStore
	called context.CancelFunc
}

func (s replayControl) ReplayCompletions(context.Context, int) (int, error) {
	s.called()
	return 1, nil
}

func TestNativeReconciliationUsesCommercialRecoveryAndKeepsRetryingDurableEvidence(t *testing.T) {
	config, _, catalogue := nativeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := 0
	config.Recover = func(bounded context.Context, limit int) error {
		calls++
		if limit != 100 {
			t.Fatal("recovery batch became unbounded")
		}
		if _, ok := bounded.Deadline(); !ok {
			t.Fatal("recovery has no deadline")
		}
		if calls == 2 {
			cancel()
			return nil
		}
		return errors.New("synthetic temporary settlement error")
	}
	h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
	if err := h.api.RunNativeReconciliation(ctx); !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatalf("durable recovery calls=%d error=%v", calls, err)
	}
}

func TestNativeInternalReconciliationRetainsExistingReplay(t *testing.T) {
	config, _, catalogue := nativeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	called := false
	config.Store = replayControl{NativeStore: config.Store, called: func() { called = true; cancel() }}
	h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
	if err := h.api.RunNativeReconciliation(ctx); !errors.Is(err, context.Canceled) || !called {
		t.Fatal("internal completion replay changed", err)
	}
}

func TestNativeCommercialFundingErrorsRemainTypedAndNeverCallAProvider(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{billing.ErrFunds, http.StatusPaymentRequired},
		{billing.ErrBudget, http.StatusPaymentRequired},
		{billing.ErrDisabled, http.StatusForbidden},
		{billing.ErrNotFound, http.StatusForbidden},
	} {
		config, store, catalogue := nativeFixture(t)
		store.admitErr = tc.err
		h := newHarness(t, harnessOptions{native: config, catalogue: catalogue})
		response := h.post("/v1/responses", validKey, map[string]any{"model": "auto", "input": "hello"})
		if response.StatusCode != tc.status || h.adapter.lastCall() != nil {
			t.Fatalf("funding failure status=%d; providerCalled=%v", response.StatusCode, h.adapter.lastCall() != nil)
		}
		h.oxy.mu.Lock()
		grants := h.oxy.grants
		h.oxy.mu.Unlock()
		if grants != 0 {
			t.Fatal("commercial refusal fell back to legacy authority")
		}
	}
}
