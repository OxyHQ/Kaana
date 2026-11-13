package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

func buildNativeAPI(deps publicAPIDependencies, getenv func(string) string) (*publicapi.NativeConfig, func(), error) {
	path := strings.TrimSpace(getenv("KAANA_NATIVE_POLICY_PATH"))
	hash := strings.TrimSpace(getenv("KAANA_NATIVE_POLICY_SHA256"))
	if path == "" && hash == "" {
		return nil, func() {}, nil
	}
	policies, err := nativeadmission.LoadPolicyFile(path, hash)
	if err != nil {
		return nil, nil, err
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
	store, closePool, err := nativeadmission.OpenRuntime(ctx, strings.TrimSpace(getenv("DATABASE_URL")))
	if err != nil {
		return nil, nil, err
	}
	return &publicapi.NativeConfig{Identity: identity, Store: store, Policies: policies, Promotions: publicapi.NativeFunding{Source: deps.store, Registry: deps.registry}}, closePool, nil
}
