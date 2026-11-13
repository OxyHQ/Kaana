package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestInspectAccessRejectsArgumentsAndSanitizesConfiguration(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"inspect-access", "--sql"}, strings.NewReader(""), &output, func(string) string {
		t.Fatal("unexpected environment read")
		return ""
	}); err == nil {
		t.Fatal("arbitrary query arguments accepted")
	}
	if err := runInspectAccess(nil, &output, func(key string) string {
		if key != "DATABASE_URL" {
			t.Fatal("unexpected configuration")
		}
		return "private malformed URL"
	}); err == nil || strings.Contains(err.Error(), "private malformed") || output.Len() != 0 {
		t.Fatal("database configuration was exposed or accepted")
	}
}

func TestPostgresInspectAccessReportsGrantedDeniedAndAbsentRoles(t *testing.T) {
	connectionURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if connectionURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(connectionURL)
	if err != nil {
		t.Fatal("invalid PostgreSQL test configuration")
	}
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("test PostgreSQL unavailable")
	}
	defer func() { _ = admin.Close(ctx) }()
	nonce := make([]byte, 8)
	if _, err = rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	prefix := "access_" + hex.EncodeToString(nonce)
	login, granted, denied, missing := prefix+"_login", prefix+"_grant", prefix+"_deny", prefix+"_missing"
	for _, role := range []string{login, granted, denied} {
		options := " NOLOGIN"
		if role == login {
			options = " LOGIN NOINHERIT"
		}
		if _, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+options); err != nil {
			t.Fatal(err)
		}
		defer func(role string) {
			if _, err := admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
		}(role)
	}
	if _, err = admin.Exec(ctx, "GRANT "+pgx.Identifier{granted}.Sanitize()+" TO "+pgx.Identifier{login}.Sanitize()+" WITH SET TRUE, INHERIT FALSE"); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{prefix}.Sanitize()+" OWNER "+pgx.Identifier{login}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{prefix}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	readConfig := config.Copy()
	readConfig.Database = prefix
	readConfig.User = login
	readConfig.RuntimeParams["default_transaction_read_only"] = "on"
	reader, err := pgx.ConnectConfig(ctx, readConfig)
	if err != nil {
		t.Fatal("read-only fixture connection failed")
	}
	defer func() { _ = reader.Close(ctx) }()
	report, err := readAccessMetadata(ctx, reader, []string{granted, denied, missing})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ReadOnly    bool   `json:"readOnly"`
		CurrentRole string `json:"currentRole"`
		Superuser   bool   `json:"currentRoleSuperuser"`
		Roles       []struct {
			Name   string `json:"name"`
			Exists bool   `json:"exists"`
			Member bool   `json:"member"`
			Set    bool   `json:"canSetRole"`
		} `json:"roles"`
	}
	if err = json.Unmarshal(report, &result); err != nil {
		t.Fatal(err)
	}
	if !result.ReadOnly || result.CurrentRole != login || result.Superuser || len(result.Roles) != 3 {
		t.Fatalf("unexpected metadata: %s", report)
	}
	for _, role := range result.Roles {
		if role.Exists != (role.Name != missing) || role.Member != (role.Name == granted) || role.Set != (role.Name == granted) {
			t.Fatalf("incorrect membership: %+v", role)
		}
	}
	if _, err = reader.Exec(ctx, "CREATE TABLE forbidden_write(id integer)"); err == nil {
		t.Fatal("inspection connection permitted mutation")
	}
}
