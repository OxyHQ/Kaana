package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// Kaana refuses partial configuration (docs/operating.md). This task holds the
// encrypt-only authority over platform provider keys, so each way it can be started
// half-configured must stop it before it listens, and none may print a secret.

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func publicKeySpec(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return "test-key:" + base64.StdEncoding.EncodeToString(public)
}

func TestItRefusesToStartWithoutItsPublicKeys(t *testing.T) {
	t.Setenv("KAANA_PLATFORM_CREDENTIAL_CONTROL_PUBLIC_KEYS", "")
	t.Setenv("DATABASE_URL", "")
	err := run(quietLogger())
	if err == nil {
		t.Fatal("a task with no signing keys started")
	}
}

func TestItRefusesToStartWithoutADatabase(t *testing.T) {
	t.Setenv("KAANA_PLATFORM_CREDENTIAL_CONTROL_PUBLIC_KEYS", publicKeySpec(t))
	t.Setenv("DATABASE_URL", "")
	err := run(quietLogger())
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("a task with no database started, or failed for another reason: %v", err)
	}
}

func TestItRefusesAnUnverifiedDatabaseConnectionWithoutEchoingThePassword(t *testing.T) {
	t.Setenv("KAANA_PLATFORM_CREDENTIAL_CONTROL_PUBLIC_KEYS", publicKeySpec(t))
	const password = "hunter2-do-not-print"
	t.Setenv("DATABASE_URL", "postgres://kaana:"+password+"@db.example.internal:5432/kaana?sslmode=disable")
	err := run(quietLogger())
	if err == nil {
		t.Fatal("a task started on a database connection without verified TLS")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("the refusal quoted the database password: %v", err)
	}
}
