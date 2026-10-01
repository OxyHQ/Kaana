package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/openrouterkey"
	"io"
	"strings"
	"testing"
)

type testExactCustody struct {
	err   error
	calls int
}

func (c *testExactCustody) WithCredential(ctx context.Context, s credentialstore.Scope, use func([]byte) error) error {
	c.calls++
	if s.Provider != "openrouter" || s.KeyID != "fixture-key" {
		return errors.New("wrong scope")
	}
	if c.err != nil {
		return c.err
	}
	return use([]byte("synthetic-key"))
}

type testKeyInspector struct {
	err    error
	report openrouterkey.Report
	calls  int
}

func (i *testKeyInspector) Inspect(_ context.Context, secret []byte, _ openrouterkey.Expectation) (openrouterkey.Report, error) {
	i.calls++
	if string(secret) != "synthetic-key" {
		return openrouterkey.Report{}, errors.New("wrong secret")
	}
	return i.report, i.err
}

type unsafeWriter struct{}

func (unsafeWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic-key writer") }
func TestInspectCLISelectors(t *testing.T) {
	good := []string{"--key-id", "fixture-key", "--expected-organization-id", "org_fixture"}
	scope, expect, err := parseInspectOpenRouterKey(good)
	if err != nil || scope.Provider != "openrouter" || expect.OrganizationID != "org_fixture" {
		t.Fatal(err)
	}
	for _, extra := range [][]string{{"--endpoint", "https://evil.example"}, {"--provider", "typesafe"}, {"--secret", "synthetic-key"}, {"positional"}} {
		args := append(append([]string{}, good...), extra...)
		_, _, err := parseInspectOpenRouterKey(args)
		if err == nil || strings.Contains(err.Error(), "synthetic-key") {
			t.Fatal("unsafe CLI accepted or leaked")
		}
	}
	for _, args := range [][]string{nil, {"--key-id", "fixture-key"}, {"--expected-organization-id", "org_fixture"}} {
		if _, _, err := parseInspectOpenRouterKey(args); err == nil {
			t.Fatal("missing selector accepted")
		}
	}
}
func TestInspectCLIOutputAndSafeErrors(t *testing.T) {
	scope := credentialstore.Scope{Provider: "openrouter", KeyID: "fixture-key"}
	expect := openrouterkey.Expectation{OrganizationID: "org_fixture"}
	for _, tc := range []struct {
		name                   string
		custodyErr, inspectErr error
		matches                bool
		writer                 io.Writer
		want                   error
	}{
		{name: "match", matches: true}, {name: "unknown", want: openrouterkey.ErrExpectationNotMet},
		{name: "database", custodyErr: errors.New("synthetic-key DB"), want: credentialstore.ErrCredentialUnavailable},
		{name: "wrapped", inspectErr: fmt.Errorf("synthetic-key: %w", openrouterkey.ErrMalformedResponse), want: openrouterkey.ErrMalformedResponse},
		{name: "unknown-error", inspectErr: errors.New("synthetic-key transport"), want: openrouterkey.ErrRequestFailed},
		{name: "writer", matches: true, writer: unsafeWriter{}, want: errInspectionOutput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &testExactCustody{err: tc.custodyErr}
			i := &testKeyInspector{err: tc.inspectErr, report: openrouterkey.Report{OrganizationMatchesExpected: tc.matches}}
			var out bytes.Buffer
			writer := tc.writer
			if writer == nil {
				writer = &out
			}
			err := inspectOpenRouterKey(context.Background(), c, i, scope, expect, writer)
			if !errors.Is(err, tc.want) {
				t.Fatalf("result: %v want %v", err, tc.want)
			}
			if c.calls != 1 || i.calls > 1 {
				t.Fatal("custody retry")
			}
			if strings.Contains(out.String(), "synthetic-key") || (err != nil && strings.Contains(err.Error(), "synthetic-key")) {
				t.Fatal("secret leak")
			}
			if tc.custodyErr != nil && i.calls != 0 {
				t.Fatal("failed custody reached inspector")
			}
		})
	}
}
