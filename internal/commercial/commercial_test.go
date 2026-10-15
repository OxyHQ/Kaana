package commercial_test

import (
	"os"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/commercial"
	"github.com/OxyHQ/Kaana/internal/contract"
)

const filePath = "../../configs/deployment-commercial.json"

func file(entries ...string) []byte {
	return []byte(`{"$comment":"test","schemaVersion":1,"deployments":[` + strings.Join(entries, ",") + `]}`)
}

func entry(id, scope, permission string, extra string) string {
	return `{"deploymentId":"` + id + `","commercial":{"availabilityScope":"` + scope + `","commercialPermission":"` + permission + `"` + extra + `}}`
}

func TestParseServesTheListedDeploymentsAndNothingElse(t *testing.T) {
	table, err := commercial.Parse(file(
		entry("dep_a", "platform_internal", "standard_application_use", ""),
		entry("dep_b", "enterprise", "wholesale_contract", `,"dedicatedCapacity":true`),
	))
	if err != nil {
		t.Fatal(err)
	}
	if table.Len() != 2 {
		t.Fatalf("len = %d", table.Len())
	}
	a := table.For("dep_a")
	if a == nil || a.AvailabilityScope != contract.ScopePlatformInternal || a.CommercialPermission != contract.PermissionStandardApplicationUse || a.DedicatedCapacity != nil {
		t.Fatalf("dep_a = %+v", a)
	}
	b := table.For("dep_b")
	if b == nil || b.DedicatedCapacity == nil || !*b.DedicatedCapacity {
		t.Fatalf("dep_b = %+v", b)
	}
	if table.For("dep_c") != nil || table.For("dep_") != nil {
		t.Fatal("an unlisted (or prefix) deployment was sold")
	}
	*b.DedicatedCapacity = false
	if again := table.For("dep_b"); !*again.DedicatedCapacity {
		t.Fatal("a returned copy aliased the table")
	}
}

func TestTheEmptyTableSellsNothing(t *testing.T) {
	var nilTable *commercial.Table
	if nilTable.For("dep_a") != nil || nilTable.Len() != 0 {
		t.Fatal("a nil table sold something")
	}
	table, err := commercial.Load("")
	if err != nil || table.For("dep_a") != nil {
		t.Fatalf("an unset path is the empty table: %v", err)
	}
}

func TestParseRefusesWhatTheContractRefuses(t *testing.T) {
	good := entry("dep_a", "platform_internal", "standard_application_use", "")
	for name, raw := range map[string][]byte{
		"unknown scope":                 file(entry("dep_a", "internal_alia", "standard_application_use", "")),
		"unknown permission":            file(entry("dep_a", "platform_internal", "free_for_all", "")),
		"byok scope without byok":       file(entry("dep_a", "byok_only", "standard_application_use", "")),
		"byok permission without scope": file(entry("dep_a", "platform_internal", "customer_byok", "")),
		"public_payg without resale":    file(entry("dep_a", "public_payg", "standard_application_use", "")),
		"unknown commercial field":      file(entry("dep_a", "platform_internal", "standard_application_use", `,"status":"active"`)),
		"duplicate id":                  file(good, good),
		"out of order":                  file(entry("dep_b", "platform_internal", "standard_application_use", ""), good),
		"no id":                         file(entry("", "platform_internal", "standard_application_use", "")),
		"unknown top-level field":       []byte(`{"$comment":"x","schemaVersion":1,"deployments":[],"extra":1}`),
		"wrong schema version":          []byte(`{"$comment":"x","schemaVersion":2,"deployments":[]}`),
		"no provenance note":            []byte(`{"schemaVersion":1,"deployments":[]}`),
		"dedicatedCapacity not a bool":  file(entry("dep_a", "platform_internal", "standard_application_use", `,"dedicatedCapacity":"yes"`)),
		"not json":                      []byte(`nope`),
	} {
		if _, err := commercial.Parse(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Positive controls: the accepted counterparts of the refusals above.
	for name, raw := range map[string][]byte{
		"byok":          file(entry("dep_a", "byok_only", "customer_byok", "")),
		"public resale": file(entry("dep_a", "public_payg", "public_resale_approved", "")),
		"open weight":   file(entry("dep_a", "public_payg", "open_weight_hosting", "")),
		"oxy hosted":    file(entry("dep_a", "oxy_hosted", "open_weight_hosting", "")),
	} {
		if _, err := commercial.Parse(raw); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The checked-in file is the one the image bakes and the process loads at
// start-up; it must parse, and it is not the empty table.
func TestTheCheckedInFileParsesAndSellsTheSeededDeployments(t *testing.T) {
	table, err := commercial.Load(filePath)
	if err != nil {
		t.Fatal(err)
	}
	// 354 exported rows, 264 approved and active: an empty or truncated file
	// would otherwise parse and sell nothing.
	if table.Len() < 264 {
		t.Fatalf("the file sells %d deployments, the seed was 264", table.Len())
	}
	for _, id := range table.IDs() {
		commercial := table.For(id)
		if err := commercial.Validate(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		if !strings.HasPrefix(string(id), "dep_") {
			t.Errorf("%q is not a deployment id", id)
		}
	}
	if table.For("dep_cerebras_gpt_oss_120b_observed_2026_09_01").AvailabilityScope != contract.ScopePlatformInternal {
		t.Error("the legacy internal_alia rows must map to platform_internal")
	}
	raw, _ := os.ReadFile(filePath)
	if strings.Contains(string(raw), "internal_alia\"") {
		t.Error("the legacy internal_alia scope is in the file")
	}
	// OpenAI transcription has no evidence of a review: it stays unsold.
	for _, id := range table.IDs() {
		if strings.HasPrefix(string(id), "dep_openai_audio_") {
			t.Errorf("%s is sold without evidence", id)
		}
	}
}
