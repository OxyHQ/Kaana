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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/oxyvalidation"
	"github.com/OxyHQ/Kaana/internal/platformactivity"
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

	var activity *platformactivity.Collector
	// Gated on the infrastructure coordinates being present, not on the Oxy
	// service credential: that credential feeds only this activity reporter
	// here, but so do the coordinates, so either could be the signal, and
	// using both invites the pair drifting out of sync. Coordinates win
	// because every activity-reporting binary in this repo, including
	// cmd/kaana where the credential is unconditionally required for
	// something else, agrees on them.
	rawLongitude := os.Getenv("KAANA_INFRASTRUCTURE_LONGITUDE")
	rawLatitude := os.Getenv("KAANA_INFRASTRUCTURE_LATITUDE")
	if rawLongitude != "" || rawLatitude != "" {
		apiKey := os.Getenv("KAANA_OXY_SERVICE_API_KEY")
		apiSecret := os.Getenv("KAANA_OXY_SERVICE_API_SECRET")
		if apiKey == "" || apiSecret == "" {
			return errors.New("kaana platform credential control activity requires KAANA_OXY_SERVICE_API_KEY and KAANA_OXY_SERVICE_API_SECRET")
		}
		validationReporter, reporterErr := oxyvalidation.New(oxyvalidation.Config{
			BaseURL:     envOr("KAANA_OXY_API_BASE_URL", "https://api.oxy.so"),
			APIKey:      apiKey,
			APISecret:   apiSecret,
			Environment: contract.Environment(envOr("KAANA_OXY_SERVICE_ENVIRONMENT", string(contract.EnvironmentProduction))),
			Logger:      logger,
		})
		if reporterErr != nil {
			return reporterErr
		}
		defer func() {
			closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if closeErr := validationReporter.Close(closeContext); closeErr != nil {
				logger.Error("platform credential control activity reporter did not drain", "errorType", "validation_shutdown")
			}
		}()
		location := os.Getenv("KAANA_INFRASTRUCTURE_LABEL")
		longitude, lonErr := strconv.ParseFloat(rawLongitude, 64)
		latitude, latErr := strconv.ParseFloat(rawLatitude, 64)
		if lonErr != nil || latErr != nil {
			return errors.New("kaana platform credential control activity requires infrastructure coordinates")
		}
		activity, err = platformactivity.New(platformactivity.Config{
			Region: os.Getenv("AWS_REGION"), Label: location, Service: "kaana-platform-credential-control",
			Coordinates: [2]float64{longitude, latitude},
			BaseURL:     envOr("KAANA_OXY_API_BASE_URL", "https://api.oxy.so"),
			Token:       validationReporter.ServiceToken, Logger: logger,
		})
		if err != nil {
			return err
		}
	}

	address := strings.TrimSpace(os.Getenv("KAANA_PLATFORM_CREDENTIAL_CONTROL_ADDR"))
	if address == "" {
		address = ":8083"
	}
	handler := server.Handler()
	if activity != nil {
		handler = activity.Middleware(handler)
	}
	httpServer := &http.Server{
		Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	if activity != nil {
		go activity.Run()
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if activity.Close(closeCtx) != nil {
				logger.Warn("platform activity shutdown timed out")
			}
		}()
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

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
