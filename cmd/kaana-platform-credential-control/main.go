// Command kaana-platform-credential-control runs the signed encrypt-only
// mutation surface for Kaana-owned provider pool credentials.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/platformcredentialcontrol"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("Kaana platform credential control could not start", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	keys, err := edgeauth.ParsePublicKeys(os.Getenv("KAANA_PLATFORM_CREDENTIAL_CONTROL_PUBLIC_KEYS"))
	if err != nil {
		return fmt.Errorf("KAANA_PLATFORM_CREDENTIAL_CONTROL_PUBLIC_KEYS: %w", err)
	}
	verifier, err := edgeauth.NewPlatformCredentialControlVerifier(keys, edgeauth.DefaultMaxSkew)
	if err != nil {
		return err
	}
	openContext, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	repository, err := credentialstore.OpenPostgres(openContext, strings.TrimSpace(os.Getenv("DATABASE_URL")))
	if err != nil {
		return err
	}
	defer repository.Close()
	cipher, err := credentialstore.OpenKMSCipher(openContext, os.Getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN"))
	if err != nil {
		return err
	}
	writer, err := credentialstore.NewPlatformWriter(repository, cipher)
	if err != nil {
		return err
	}
	server, err := platformcredentialcontrol.New(verifier, writer, logger)
	if err != nil {
		return err
	}
	address := strings.TrimSpace(os.Getenv("KAANA_PLATFORM_CREDENTIAL_CONTROL_ADDR"))
	if address == "" {
		address = ":8083"
	}
	httpServer := &http.Server{
		Addr: address, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	failed := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()
	logger.Info("Kaana platform credential control is listening", "address", address, "edgeKeyIds", verifier.KeyIDs())
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		return httpServer.Shutdown(shutdownContext)
	}
}
