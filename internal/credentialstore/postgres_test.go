package credentialstore

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresRequiresVerifiedTLSForEveryConnectionAttempt(t *testing.T) {
	for name, databaseURL := range map[string]string{
		"disabled":                     "postgres://user:secret@db.example.invalid/kaana?sslmode=disable",
		"preferred":                    "postgres://user:secret@db.example.invalid/kaana?sslmode=prefer",
		"encrypted but unverified":     "postgres://user:secret@db.example.invalid/kaana?sslmode=require",
		"certificate without hostname": "postgres://user:secret@db.example.invalid/kaana?sslmode=verify-ca",
	} {
		t.Run(name, func(t *testing.T) {
			config, err := pgxpool.ParseConfig(databaseURL)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if err := requireVerifiedPostgresTLS(config); err == nil {
				t.Fatal("unverified PostgreSQL transport was accepted")
			}
		})
	}

	config, err := pgxpool.ParseConfig("postgres://user:secret@db.example.invalid/kaana?sslmode=verify-full&sslrootcert=system")
	if err != nil {
		t.Fatalf("ParseConfig verified URL: %v", err)
	}
	if err := requireVerifiedPostgresTLS(config); err != nil {
		t.Fatalf("verified PostgreSQL transport was refused: %v", err)
	}
}
