package peablebilling

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func operatorConfigBytes() []byte {
	b, _ := json.Marshal(OperatorConfig{Enabled: true, ApplicationID: "app-kaana", OwnerAccountID: "owner-platform", MerchantID: "merch_kaana", Environment: "production"})
	return b
}
func TestReviewedOperatorConfig(t *testing.T) {
	valid := operatorConfigBytes()
	for _, tc := range []struct {
		name       string
		data       []byte
		changeHash bool
		valid      bool
	}{
		{"valid", valid, false, true},
		{"disabled", []byte(strings.Replace(string(valid), `"enabled":true`, `"enabled":false`, 1)), false, false},
		{"changed review", valid, true, false},
		{"unknown", []byte(strings.Replace(string(valid), `{`, `{"payer":"caller",`, 1)), false, false},
		{"duplicate", []byte(strings.Replace(string(valid), `{`, `{"merchantId":"other",`, 1)), false, false},
		{"missing owner", []byte(strings.Replace(string(valid), `"owner-platform"`, `""`, 1)), false, false},
		{"wrong environment", []byte(strings.Replace(string(valid), `"production"`, `"test"`, 1)), false, false},
		{"trailing", append(append([]byte{}, valid...), []byte(` {}`)...), false, false},
		{"null", []byte(strings.Replace(string(valid), `"enabled":true`, `"enabled":null`, 1)), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(p, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256(tc.data)
			if tc.changeHash {
				h[0]++
			}
			_, err := ReadOperatorConfig(p, hex.EncodeToString(h[:]))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
func TestOperatorDocuments(t *testing.T) {
	for _, raw := range []string{
		`{"action":"migrate"}`, `{"action":"bind","accountId":"a","customerId":"c"}`, `{"action":"prepare","accountId":"a","key":"k","receiptIds":["r"],"daysUntilDue":30}`, `{"action":"inspect","draftId":"d"}`, `{"action":"deliver","draftId":"d"}`, `{"action":"reconcile-event","eventId":"evt"}`,
	} {
		if _, err := ReadOperation(strings.NewReader(raw)); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{
		`{"action":"pay","draftId":"d"}`, `{"action":"send","draftId":"d"}`, `{"action":"deliver","draftId":"d","accountId":"other"}`, `{"action":"migrate","daysUntilDue":1}`, `{"action":"prepare","accountId":"a","key":"k","receiptIds":["r","r"],"daysUntilDue":30}`, `{"action":"prepare","accountId":"a","key":"k","receiptIds":["r"]}`, `{"action":"deliver","draftId":null}`, `{"action":"deliver","draftId":"a","draftId":"b"}`, `{"action":"bind","accountId":"a","customerId":"c","merchantId":"other"}`, `{"action":"inspect","draftId":"d"} {}`, `{"action":"inspect","draftId":"\nsecret"}`,
	} {
		if _, err := ReadOperation(strings.NewReader(raw)); !errors.Is(err, ErrMismatch) {
			t.Fatalf("accepted invalid operation %s: %v", raw, err)
		}
	}
}
