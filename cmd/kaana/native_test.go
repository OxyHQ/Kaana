package main

import "testing"

func TestNativeAbsentIsDisabledAndPartialConfigRefusesBoot(t *testing.T) {
	config, closePool, err := buildNativeAPI(publicAPIDependencies{}, func(string) string { return "" })
	if err != nil || config != nil || closePool == nil {
		t.Fatal("absence is not disabled", err)
	}
	closePool()
	for _, present := range []string{"KAANA_NATIVE_POLICY_PATH", "KAANA_NATIVE_POLICY_SHA256"} {
		_, _, err := buildNativeAPI(publicAPIDependencies{}, func(name string) string {
			if name == present {
				return "incomplete"
			}
			return ""
		})
		if err == nil {
			t.Fatal("partial activation configuration ignored")
		}
	}
}

func TestNativeCommercialRequiresExplicitConfiguration(t *testing.T) {
	for _, raw := range []string{"1", "TRUE", "yes", "enabled"} {
		if _, err := nativeCommercialEnabled(raw); err == nil {
			t.Fatal("ambiguous commercial configuration accepted")
		}
	}
	for _, raw := range []string{"", "false"} {
		if enabled, err := nativeCommercialEnabled(raw); err != nil || enabled {
			t.Fatal("commercial admission enabled by absence")
		}
	}
	if enabled, err := nativeCommercialEnabled("true"); err != nil || !enabled {
		t.Fatal("explicit enablement refused")
	}
	_, _, err := buildNativeAPI(publicAPIDependencies{}, func(name string) string {
		if name == "KAANA_NATIVE_COMMERCIAL_ENABLED" {
			return "true"
		}
		return ""
	})
	if err == nil {
		t.Fatal("commercial configuration without reviewed policy accepted")
	}
}
