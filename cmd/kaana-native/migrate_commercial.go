package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/OxyHQ/Kaana/internal/commercialadmission"
)

func runMigrateCommercial(args []string, output io.Writer, getenv func(string) string, migrate func(context.Context, string) error) error {
	if len(args) != 0 {
		return errors.New("migrate-commercial accepts no arguments")
	}
	if getenv("DATABASE_URL") == "" {
		return errors.New("commercial migration database configuration is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate(ctx, getenv("DATABASE_URL")); err != nil {
		return errors.New("commercial schema preparation refused; inspect prerequisites without enabling serving")
	}
	return json.NewEncoder(output).Encode(map[string]string{"status": "complete", "operation": "migrate-commercial"})
}

func migrateCommercial(args []string, output io.Writer, getenv func(string) string) error {
	return runMigrateCommercial(args, output, getenv, commercialadmission.MigrateDatabase)
}
