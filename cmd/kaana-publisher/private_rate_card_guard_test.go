package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"testing"
)

// Exercise the actual startup guard: standalone Auto must reach the shared
// loader, while neither authority must retain optional ordinary-card behavior.
func TestPublisherLoadsCardsForEitherIndependentAuthority(t *testing.T) {
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	guards := 0
	ast.Inspect(file, func(node ast.Node) bool {
		statement, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		var body bytes.Buffer
		if err := printer.Fprint(&body, fs, statement.Body); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body.Bytes(), []byte("scopedpermit.LoadRateCards(")) {
			return true
		}
		var condition bytes.Buffer
		if err := printer.Fprint(&condition, fs, statement.Cond); err != nil {
			t.Fatal(err)
		}
		if condition.String() != "scopedpermit.SourceReviewedAudience() != nil || scopedpermit.SourceReviewedPrivateAutoApproval() != nil" {
			t.Fatalf("private cards startup guard differs: %s", condition.String())
		}
		guards++
		return false
	})
	if guards != 1 {
		t.Fatalf("wanted one exact shared private loader guard, got %d", guards)
	}
}
