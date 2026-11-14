package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/billingaccounts"
)

type fakeAccountOperator struct {
	calls int
	fail  bool
}

func (s *fakeAccountOperator) Migrate(context.Context) error { s.calls++; return nil }
func (s *fakeAccountOperator) Apply(ctx context.Context, c billingaccounts.Config, o billingaccounts.Operation) (json.RawMessage, error) {
	s.calls++
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 2*time.Minute {
		return nil, errors.New("missing bound")
	}
	if s.fail {
		return nil, errors.New("private database error must not leak")
	}
	return json.RawMessage(`{"action":"provision","snapshot":{"accountId":"kba_test","enabled":false}}`), nil
}
func TestBillingAccountCLIReviewBeforeAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"enabled":true,"ownerAccountId":"owner","currency":"USD"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	args := []string{"--config-file", path, "--config-sha256", hash}
	opened := 0
	s := &fakeAccountOperator{}
	open := func(context.Context, string, billingaccounts.Operation) (accountOperator, func(), error) {
		opened++
		return s, func() {}, nil
	}
	for _, raw := range []string{`{"action":"fund"}`, `{"action":"provision","key":"k"}`, `{"action":"configure","enabled":true}`, `{"action":"migrate","amount":"100"}`} {
		if err := runBillingAccounts(args, strings.NewReader(raw), &bytes.Buffer{}, func(string) string { return "" }, open); err == nil {
			t.Fatal("invalid action accepted")
		}
	}
	badArgs := append([]string{}, args...)
	badArgs[3] = strings.Repeat("a", 64)
	if err := runBillingAccounts(badArgs, strings.NewReader(`{"action":"migrate"}`), &bytes.Buffer{}, func(string) string { return "" }, open); err == nil || opened != 0 {
		t.Fatal("opened before review")
	}
	op := filepath.Join(t.TempDir(), "op.json")
	if err := os.WriteFile(op, []byte(`{"action":"provision","key":"k","evidenceId":"review"}`), 0600); err != nil {
		t.Fatal(err)
	}
	fileArgs := append(append([]string{}, args...), "--operation-file", op)
	var output bytes.Buffer
	if err := runBillingAccounts(fileArgs, strings.NewReader("invalid unused stdin"), &output, func(string) string { return "private" }, open); err != nil || s.calls != 1 || opened != 1 {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"enabled":false`) {
		t.Fatal(output.String())
	}
	s.fail = true
	err := runBillingAccounts(fileArgs, strings.NewReader(""), &bytes.Buffer{}, func(string) string { return "private" }, open)
	if err == nil || strings.Contains(err.Error(), "database error") {
		t.Fatal("private error leaked", err)
	}
}
