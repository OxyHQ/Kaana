package contract

import (
	"reflect"
	"strings"
	"testing"
)

func chargedGenerationFixtures() ([]fixture, []fixture) {
	gen := GenerationID("generation-charge")
	user := UserID("delegated-person")
	record := ChargedGenerationRecord{SchemaVersion: 1, Kind: "charge", RequestID: "request-charge", GenerationID: &gen, ApplicationID: "application", CredentialID: "credential", DelegatedUserID: &user, Environment: EnvironmentProduction, Outcome: OutcomeCompleted, UsageSource: UsageProviderReported, Units: []UsageQuantity{{Unit: UnitOutputTokens, Quantity: 3}}, Model: "fixture/medium@v1", Labels: RequestLabels{"feature": "billing-test"}, SettledAt: "2026-11-13T12:00:00Z", ReceiptID: "kreceipt_fixture", PriceSnapshot: PriceSnapshot{PriceVersionID: "price-v1", Currency: "USD", UnitPrices: []UnitPrice{{Unit: UnitOutputTokens, Amount: "0.000001", Per: 1, Currency: "USD"}}}, BilledAmount: "0.000003000000", Currency: "USD", PlatformFeeOnly: false}
	valid := []fixture{{Schema: "chargedGenerationRecordSchema", Case: "native commercial receipt", Value: record}}
	empty := record
	empty.Outcome = OutcomeFailed
	empty.Units = []UsageQuantity{}
	empty.BilledAmount = "0.000000000000"
	valid = append(valid, fixture{Schema: "chargedGenerationRecordSchema", Case: "failed request releases without charge", Value: empty})
	currency := record
	currency.Currency = "EUR"
	duplicate := record
	duplicate.Units = append(append([]UsageQuantity{}, record.Units...), record.Units[0])
	amount := record
	amount.BilledAmount = "0.0000000000001"
	id := record
	id.ReceiptID = strings.Repeat("x", 129)
	per := record
	per.PriceSnapshot.UnitPrices = append([]UnitPrice{}, record.PriceSnapshot.UnitPrices...)
	per.PriceSnapshot.UnitPrices[0].Per = 9007199254740992
	invalid := []fixture{{Schema: "chargedGenerationRecordSchema", Case: "charge and tariff currency differ", Value: currency}, {Schema: "chargedGenerationRecordSchema", Case: "charge units duplicated", Value: duplicate}, {Schema: "chargedGenerationRecordSchema", Case: "charge exceeds exact decimal scale", Value: amount}, {Schema: "chargedGenerationRecordSchema", Case: "receipt id exceeds bound", Value: id}, {Schema: "chargedGenerationRecordSchema", Case: "price denominator is not a safe integer", Value: per}}
	return valid, invalid
}
func TestChargedGenerationValidationAndDrift(t *testing.T) {
	valid, invalid := chargedGenerationFixtures()
	for _, f := range valid {
		if err := f.Value.(ChargedGenerationRecord).Validate(); err != nil {
			t.Fatalf("%s: %v", f.Case, err)
		}
	}
	for _, f := range invalid {
		if err := f.Value.(ChargedGenerationRecord).Validate(); err == nil {
			t.Fatalf("accepted %s", f.Case)
		}
	}
	d := loadDescriptor(t)
	checker := shapeChecker{descriptor: d}
	node := d.Shapes["chargedGenerationRecordSchema"]
	if problems := checker.compareShape("chargedGenerationRecordSchema", node, reflect.TypeOf(ChargedGenerationRecord{})); len(problems) != 0 {
		t.Fatal(problems)
	}
	changed := cloneNode(t, node)
	dropField(&changed, "billedAmount")
	if reflect.DeepEqual(changed, node) {
		t.Fatal("mutation did not apply")
	}
	if problems := checker.compareShape("chargedGenerationRecordSchema", changed, reflect.TypeOf(ChargedGenerationRecord{})); len(problems) == 0 {
		t.Fatal("removed charge escaped drift control")
	}
}
