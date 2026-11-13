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
