package contract

import (
	"reflect"
	"strings"
	"testing"
)

func meteredGenerationFixtures() ([]fixture, []fixture) {
	generation := GenerationID("gen-native")
	user := UserID("person")
	parent := RequestID("parent")
	amount := ExactDecimal("0.000000000001")
	currency := CurrencyCode("USD")
	version := "tariff-native-1"
	value := MeteredGenerationRecord{SchemaVersion: 1, Kind: "metered_usage", RequestID: "req-native", GenerationID: &generation, ApplicationID: "application", CredentialID: "credential", DelegatedUserID: &user, Environment: EnvironmentProduction, Outcome: OutcomeCompleted, UsageSource: UsageProviderReported, Units: []UsageQuantity{{Unit: UnitOutputTokens, Quantity: 3}}, Model: "fixture/medium@v1", Labels: RequestLabels{"feature": "test"}, SettledAt: "2026-11-13T12:00:00Z", MeteredUsageID: "kad_native", ParentRequestID: &parent, Tariff: MeteredTariff{Status: "quoted", Amount: &amount, Currency: &currency, PriceVersionID: &version}, CustomerCharge: MeteredCustomerCharge{Status: "not_charged"}}
	valid := []fixture{{Schema: "meteredGenerationRecordSchema", Case: "native quoted generation", Value: value}}
	unpriced := value
	unpriced.Tariff = MeteredTariff{Status: "unpriced"}
	valid = append(valid, fixture{Schema: "meteredGenerationRecordSchema", Case: "native unpriced generation", Value: unpriced})
	badQuoted := value
	badQuoted.Tariff.PriceVersionID = nil
	badUnpriced := unpriced
	badUnpriced.Tariff.Amount = &amount
	invalid := []fixture{{Schema: "meteredGenerationRecordSchema", Case: "quoted requires tariff version", Value: badQuoted}, {Schema: "meteredGenerationRecordSchema", Case: "unpriced cannot invent zero amount", Value: badUnpriced}}
	return valid, invalid
}

func TestMeteredTariffNestedUnionDrift(t *testing.T) {
	descriptor := loadDescriptor(t)
	checker := shapeChecker{descriptor: descriptor}
	node := descriptor.Shapes["meteredGenerationRecordSchema"]
	if problems := checker.compareShape("meteredGenerationRecordSchema", node, reflect.TypeOf(MeteredGenerationRecord{})); len(problems) != 0 {
		t.Fatal(problems)
	}
	changed := cloneNode(t, node)
	editField(&changed, "tariff", func(field *descriptorNode) {
		for i := range field.Variants {
			editField(&field.Variants[i], "priceVersionId", func(price *descriptorNode) { price.Nullable = false })
		}
	})
	if reflect.DeepEqual(node, changed) {
		t.Fatal("nullable mutation did not apply")
	}
	problems := checker.compareShape("meteredGenerationRecordSchema", changed, reflect.TypeOf(MeteredGenerationRecord{}))
	if !strings.Contains(strings.Join(problems, "\n"), "is a pointer") {
		t.Fatalf("required non-null nested value drift escaped: %v", problems)
	}
	changed = cloneNode(t, node)
	editField(&changed, "tariff", func(field *descriptorNode) {
		for i := range field.Variants {
			dropField(&field.Variants[i], "priceVersionId")
		}
	})
	if reflect.DeepEqual(node, changed) {
		t.Fatal("nested field mutation did not apply")
	}
	if problems = checker.compareShape("meteredGenerationRecordSchema", changed, reflect.TypeOf(MeteredGenerationRecord{})); len(problems) == 0 {
		t.Fatal("nested removed field drift escaped")
	}
}
