package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/commercialadmission"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

func buildNativeAPI(deps publicAPIDependencies, getenv func(string) string) (*publicapi.NativeConfig, func(), error) {
	path := strings.TrimSpace(getenv("KAANA_NATIVE_POLICY_PATH"))
	hash := strings.TrimSpace(getenv("KAANA_NATIVE_POLICY_SHA256"))
	commercialEnabled, err := nativeCommercialEnabled(getenv("KAANA_NATIVE_COMMERCIAL_ENABLED"))
	if err != nil {
		return nil, nil, err
	}
	if path == "" && hash == "" {
		if commercialEnabled {
			return nil, nil, errors.New("commercial admission requires explicit native policy configuration")
		}
		return nil, func() {}, nil
	}
	policies, err := nativeadmission.LoadPolicyFile(path, hash)
	if err != nil {
		return nil, nil, err
	}
	if !commercialEnabled {
		for _, policy := range policies {
			if policy.EconomicTreatment == nativeadmission.Commercial {
				return nil, nil, errors.New("commercial policy requires explicit commercial runtime configuration")
			}
		}
	}
	identity, err := publicapi.NewOxyIdentityVerifier(publicapi.OxyIdentityConfig{Oxy: deps.oxy, Keys: deps.keys, Logger: deps.logger})
	if err != nil {
		return nil, nil, err
	}
	if deps.store == nil || deps.registry == nil {
		return nil, nil, errors.New("native admission requires local credential evidence and registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if commercialEnabled {
		runtime, err := commercialadmission.OpenRuntime(ctx, strings.TrimSpace(getenv("DATABASE_URL")))
		if err != nil {
			return nil, nil, err
		}
		return &publicapi.NativeConfig{Commercial: deps.commercial, Identity: identity, Store: runtime.Store, Policies: policies, Promotions: publicapi.NativeFunding{Source: deps.store, Registry: deps.registry}, Recover: runtime.Recover}, runtime.Close, nil
	}
	store, closePool, err := nativeadmission.OpenRuntime(ctx, strings.TrimSpace(getenv("DATABASE_URL")))
	if err != nil {
		return nil, nil, err
	}
	return &publicapi.NativeConfig{Commercial: deps.commercial, Identity: identity, Store: store, Policies: policies, Promotions: publicapi.NativeFunding{Source: deps.store, Registry: deps.registry}}, closePool, nil
}

func nativeCommercialEnabled(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("KAANA_NATIVE_COMMERCIAL_ENABLED must be true or false")
	}
}
