package contract

import (
	"strconv"
	"strings"
)

// CatalogueAddition is one entry of the contract's CATALOGUE_ADDITIONS
// (contract set 8.17.0): a value GET /v1/models gained in set Since, which a
// reader on an earlier set refuses, so it is left out of what that reader is
// sent. Path names a field of a model, crossing any list on the way; without
// Member the field is the addition, with it the enum member is, and the list
// element holding it is left out. When that empties the list, the field at
// Emptied goes with it.
type CatalogueAddition struct {
	Since   string   `json:"since"`
	Path    []string `json:"path"`
	Member  string   `json:"member,omitempty"`
	Emptied []string `json:"emptied,omitempty"`
}

// CatalogueAdditions restates CATALOGUE_ADDITIONS; descriptor_test.go holds it
// equal to the published constant.
var CatalogueAdditions = []CatalogueAddition{
	{Since: "8.4.0", Path: []string{"capabilities", "unreported"}},
	{Since: "8.4.0", Path: []string{"pricing", "unitPrices", "unit"}, Member: string(UnitCacheWriteInputTokens), Emptied: []string{"pricing"}},
	{Since: "8.16.0", Path: []string{"capabilities", "realtime", "sessionKinds"}, Member: string(RealtimeLive), Emptied: []string{"capabilities", "realtime"}},
	{Since: "8.16.0", Path: []string{"capabilities", "realtime", "defaultKinds"}},
}

// ContractSet is a contract set's MAJOR.MINOR.PATCH, ordered numerically. The
// zero value precedes every published set: it is what a caller that states
// none is answered in.
type ContractSet [3]int

// CurrentContractSet is ContractVersion, the set this build speaks.
var CurrentContractSet = mustContractSet(ContractVersion)

// StatedContractSet is the set a request's Kaana-Contract-Version states, or
// the zero set when it states none or one that is not MAJOR.MINOR.PATCH. A
// version is read only from the header, never inferred from anything else.
func StatedContractSet(header string) ContractSet {
	set, _ := ParseContractSet(header)
	return set
}

// ParseContractSet reads MAJOR.MINOR.PATCH, each a decimal without a sign or
// leading zero.
func ParseContractSet(text string) (ContractSet, bool) {
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return ContractSet{}, false
	}
	var set ContractSet
	for index, part := range parts {
		if part == "" || len(part) > 9 || (len(part) > 1 && part[0] == '0') || strings.TrimLeft(part, "0123456789") != "" {
			return ContractSet{}, false
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return ContractSet{}, false
		}
		set[index] = value
	}
	return set, true
}

// Before reports whether s is an earlier set than other.
func (s ContractSet) Before(other ContractSet) bool {
	for index := range s {
		if s[index] != other[index] {
			return s[index] < other[index]
		}
	}
	return false
}

func mustContractSet(text string) ContractSet {
	set, ok := ParseContractSet(text)
	if !ok {
		panic("contract: " + text + " is not MAJOR.MINOR.PATCH")
	}
	return set
}
