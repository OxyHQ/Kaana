package contract

import "testing"

// A stated set orders numerically (8.16.0 is after 8.2.0, which a string
// comparison gets backwards), and anything that is not MAJOR.MINOR.PATCH is
// the zero set: before every set, so it is sent no addition.
func TestAStatedContractSetOrdersNumerically(t *testing.T) {
	for _, before := range [][2]string{{"8.2.0", "8.16.0"}, {"8.16.0", "8.16.1"}, {"7.99.99", "8.0.0"}, {"", "4.0.0"}, {"8.16", "4.0.0"}} {
		if !StatedContractSet(before[0]).Before(StatedContractSet(before[1])) {
			t.Errorf("%q is not before %q", before[0], before[1])
		}
	}
	for _, same := range []string{"8.16.0", "9.0.0"} {
		if StatedContractSet(same).Before(StatedContractSet(same)) {
			t.Errorf("%q is before itself", same)
		}
	}
	for _, unread := range []string{"", "8.16", "8.16.0.1", "v8.16.0", "8.016.0", "8.-1.0", "8. 1.0", "8.1.0-beta", "1234567890.0.0"} {
		if set, ok := ParseContractSet(unread); ok || set != (ContractSet{}) {
			t.Errorf("%q parsed as %v", unread, set)
		}
	}
	if set, ok := ParseContractSet("8.16.0"); !ok || set != (ContractSet{8, 16, 0}) {
		t.Errorf("8.16.0 parsed as %v", set)
	}
	if CurrentContractSet != StatedContractSet(ContractVersion) {
		t.Errorf("CurrentContractSet is %v; ContractVersion is %s", CurrentContractSet, ContractVersion)
	}
}
