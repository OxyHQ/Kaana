package contract

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

//go:embed scoped_execution_golden.json
var scopedExecutionGolden []byte

func TestScopedCanonicalInputGoldensMatchExactSignedUTF8(t *testing.T) {
	var golden struct {
		CanonicalInputs []struct {
			Name      string `json:"name"`
			Canonical string `json:"canonical"`
			SHA256    string `json:"sha256"`
		} `json:"canonicalInputs"`
	}
	if err := json.Unmarshal(scopedExecutionGolden, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.CanonicalInputs) < 2 {
		t.Fatal("missing cross-language cases")
	}
	for _, fixture := range golden.CanonicalInputs {
		t.Run(fixture.Name, func(t *testing.T) {
			sum := sha256.Sum256([]byte(fixture.Canonical))
			if hex.EncodeToString(sum[:]) != fixture.SHA256 {
				t.Fatal("different frozen UTF8 digest")
			}
			request := Request{ScopedExecution: &ScopedExecution{ScopedExecutionAudience: ScopedExecutionAudience{FixtureSHA256: fixture.SHA256}}}
			body := []byte(`{"schemaVersion":3,"input":` + fixture.Canonical + `}`)
			if err := request.ValidateScopedInputBytes(body); err != nil {
				t.Fatal("signed raw input hash differs from TS", err)
			}
			changed := strings.Replace(string(body), `"input":`, `"input": `, 1)
			if err := request.ValidateScopedInputBytes([]byte(changed)); err != nil {
				t.Fatal("outside-value whitespace changed input", err)
			}
		})
	}
}

func TestScopedSchemaFrozenParityMatrix(t *testing.T) {
	type parityCase struct {
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
		Valid bool            `json:"valid"`
	}
	var golden struct {
		AudienceCases []parityCase `json:"audienceCases"`
		EnvelopeCases []parityCase `json:"envelopeCases"`
	}
	if err := json.Unmarshal(scopedExecutionGolden, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.AudienceCases) == 0 || len(golden.EnvelopeCases) == 0 {
		t.Fatal("missing parity cases")
	}
	for _, fixture := range golden.AudienceCases {
		t.Run("audience/"+fixture.Name, func(t *testing.T) {
			var audience ScopedExecutionAudience
			err := json.Unmarshal(fixture.Value, &audience)
			if (err == nil) != fixture.Valid {
				t.Fatalf("TS valid=%t Go error=%v", fixture.Valid, err)
			}
		})
	}
	for _, fixture := range golden.EnvelopeCases {
		t.Run("envelope/"+fixture.Name, func(t *testing.T) {
			var request Request
			err := json.Unmarshal(fixture.Value, &request)
			if err == nil {
				err = request.Validate()
			}
			if (err == nil) != fixture.Valid {
				t.Fatalf("TS valid=%t Go error=%v", fixture.Valid, err)
			}
		})
	}
}
