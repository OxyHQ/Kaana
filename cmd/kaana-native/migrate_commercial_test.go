package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMigrateCommercialIsExplicitBoundedAndRedactsDatabaseErrors(t *testing.T) {
	for _, args := range [][]string{{"--enable"}, {"--policy-file", "anything"}} {
		if err := runMigrateCommercial(args, new(bytes.Buffer), func(string) string { return "configured" }, func(context.Context, string) error {
			t.Fatal("invalid command opened a database")
			return nil
		}); err == nil {
			t.Fatal("unknown migration arguments accepted")
		}
	}
	var output bytes.Buffer
	called := false
	err := runMigrateCommercial(nil, &output, func(string) string { return "configured" }, func(ctx context.Context, _ string) error {
		called = true
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded migration")
		}
		return nil
	})
	if err != nil || !called || !strings.Contains(output.String(), `"operation":"migrate-commercial"`) {
		t.Fatal("migration command not connected", err)
	}
	output.Reset()
	err = runMigrateCommercial(nil, &output, func(string) string { return "configured" }, func(context.Context, string) error { return errors.New("private database detail") })
	if err == nil || strings.Contains(err.Error(), "private") || output.Len() != 0 {
		t.Fatal("failed migration emitted success or private data")
	}
}
