package contract

import (
	"encoding/json"
	"testing"
)

// The rerank operation (contract set 8.11.0) is declared before anything emits
// it: it is valid, it is the last of the list (Oxy compares the list in order),
// and it survives a JSON round trip.
func TestTheRerankOperationIsDeclaredLast(t *testing.T) {
	if !GrantRerank.Valid() {
		t.Fatal("rerank is not a valid grant operation")
	}
	if last := grantOperationValues[len(grantOperationValues)-1]; last != GrantRerank {
		t.Fatalf("the last grant operation is %q, want rerank", last)
	}
	encoded, err := json.Marshal(GrantRerank)
	if err != nil {
		t.Fatal(err)
	}
	var decoded GrantOperation
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != GrantRerank || string(encoded) != `"rerank"` {
		t.Fatalf("round trip gave %s -> %q", encoded, decoded)
	}
}
