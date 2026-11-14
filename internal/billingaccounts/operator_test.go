package billingaccounts

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewedDocuments(t *testing.T) {
	raw := `{"enabled":true,"ownerAccountId":"opaque-owner","currency":"USD"}`
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	c, err := ReadConfig(p, hash)
	if err != nil || c.Digest != hash {
		t.Fatal(err)
	}
	if _, err = ReadConfig(p, strings.Repeat("a", 64)); err == nil {
		t.Fatal("changed config accepted")
	}
	for _, raw := range []string{`{"enabled":false,"ownerAccountId":"owner","currency":"USD"}`, `{"enabled":true,"enabled":true,"ownerAccountId":"owner","currency":"USD"}`, `{"enabled":true,"Enabled":true,"ownerAccountId":"owner","currency":"USD"}`, `{"enabled":true,"ownerAccountId":"owner","currency":"USD","digest":"fake"}`, `{"enabled":true,"ownerAccountId":null,"currency":"USD"}`} {
		if err = os.WriteFile(p, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		sum = sha256.Sum256([]byte(raw))
		if _, err = ReadConfig(p, hex.EncodeToString(sum[:])); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	good := `{"action":"configure","accountId":"account","key":"op","evidenceId":"review","revision":"` + strings.Repeat("a", 64) + `","budgetKey":"budget","startsAt":"2026-11-14T00:00:00.000Z","endsAt":"2026-11-15T00:00:00.000Z","amount":"10.000000000001","creditLimit":"0","enabled":false}`
	if _, err = ReadOperation(strings.NewReader(good)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{strings.Replace(good, `,"enabled":false`, "", 1), strings.Replace(good, `"enabled":false`, `"enabled":null`, 1), strings.Replace(good, `"amount":"10.000000000001"`, `"amount":10`, 1), strings.Replace(good, `.000Z`, `.000000123Z`, 1), strings.Replace(good, `"key":"op"`, `"key":"op","key":"other"`, 1), `{"action":"fund","amount":"10"}`, `{"action":"migrate","enabled":false}`, `{"action":"inspect","accountId":"a","key":"unused"}`, `{"action":"provision","key":"op"}`, good + ` {}`} {
		if _, err = ReadOperation(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
