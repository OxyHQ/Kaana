package nativeadmission

import (
	"reflect"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

const reviewedLivePolicySHA = "bb80ef1fb91fc589d75c99446631c18e53100031de9c68c90e17d64776e6249b"

func reviewedLivePolicy(t *testing.T) Policy {
	t.Helper()
	policies, err := LoadPolicyFile("../../configs/native-admission/alia-kaana-v3.json", reviewedLivePolicySHA)
	if err != nil || len(policies) != 1 {
		t.Fatal("reviewed live policy invalid", err)
	}
	return policies[0]
}

func TestReviewedLivePolicyChangesOnlyVersionAndKind(t *testing.T) {
	old, err := LoadPolicyFile("../../configs/native-admission/alia-kaana-v2.json", "eb3385f238ff5639771542b0caeb6cb7474347d59f205400351e6f9c2adcf490")
	if err != nil {
		t.Fatal(err)
	}
	current := reviewedLivePolicy(t)
	if current.Version != 3 || !reflect.DeepEqual(current.Realtime.SessionKinds, []contract.RealtimeSessionKind{contract.RealtimeConversation, contract.RealtimeTranscription, contract.RealtimeLive}) {
		t.Fatal("unexpected version or live kinds")
	}
	digest, err := PolicyDigest(current)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reviewed live policy digest: %s", digest)
	current.Version = old[0].Version
	current.Realtime.SessionKinds = old[0].Realtime.SessionKinds
	if !reflect.DeepEqual(current, old[0]) {
		t.Fatal("identity, tariffs, limits or other policy terms changed")
	}
}

func TestPostgresReviewedLiveRegistrationLeavesExistingEnrollmentDisabled(t *testing.T) {
	s, _, ctx := testDatabase(t)
	old, err := LoadPolicyFile("../../configs/native-admission/alia-kaana-v2.json", "eb3385f238ff5639771542b0caeb6cb7474347d59f205400351e6f9c2adcf490")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterPolicy(ctx, old[0]); err != nil {
		t.Fatal(err)
	}
	current := reviewedLivePolicy(t)
	for i := 0; i < 2; i++ {
		if err = s.RegisterPolicy(ctx, current); err != nil {
			t.Fatal(err)
		}
		report, err := s.InspectPolicy(ctx, current)
		if err != nil || !report.Registered || report.Enabled || report.ActivePolicy != nil || report.LegacyFenceComplete {
			t.Fatal("registration activated or changed enrollment", report, err)
		}
	}
	previous, err := s.InspectPolicy(ctx, old[0])
	if err != nil || !previous.Registered || previous.Enabled {
		t.Fatal("historical registration changed", previous, err)
	}
}
