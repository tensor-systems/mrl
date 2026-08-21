package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testRLMProfileProjectID = "11111111-1111-4111-8111-111111111111"
	testRLMProfileTierID    = "22222222-2222-4222-8222-222222222222"
)

func TestTierPresetUpdateRLMProfile_UsesReviewedUpdateOnlyContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		wantPath := "/projects/" + testRLMProfileProjectID + "/tiers/" + testRLMProfileTierID +
			"/presets/balanced/rlm-profile"
		if request.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", request.URL.Path, wantPath)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer account-token" {
			t.Errorf("authorization = %q", authorization)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(payload) != 3 || payload["profile"] != "cozy-launch" ||
			payload["expect_current_kind"] != "single" ||
			payload["expect_current_model"] != "gpt-5.6-terra" {
			t.Errorf("payload = %#v", payload)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"changed":true,
			"profile":"cozy-launch",
			"preset":{"preset_code":"balanced"},
			"revision":{
				"revision_id":"cozy-launch-v1",
				"content_hash":"abc123",
				"root":{"model":"gpt-5.6-terra"},
				"subcall":{"model":"gpt-5.6-luna"}
			}
		}`))
	}))
	defer server.Close()

	command := newTierPresetUpdateRLMProfileCmd()
	command.SetContext(withRuntimeConfig(t.Context(), runtimeConfig{
		BaseURL: server.URL, ProjectID: testRLMProfileProjectID,
		Token: "account-token", Output: outputFormatJSON, Timeout: time.Second,
	}))
	command.SetArgs([]string{
		"balanced", "--tier", testRLMProfileTierID, "--profile", "cozy-launch",
		"--expect-current-kind", "single", "--expect-current-model", "gpt-5.6-terra",
	})
	if err := command.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
}

func TestTierPresetUpdateRLMProfile_RejectsInvalidLockedExpectationBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	command := newTierPresetUpdateRLMProfileCmd()
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetContext(withRuntimeConfig(t.Context(), runtimeConfig{
		BaseURL: server.URL, ProjectID: testRLMProfileProjectID,
		Token: "account-token", Timeout: time.Second,
	}))
	command.SetArgs([]string{
		"balanced", "--tier", testRLMProfileTierID, "--profile", "cozy-launch",
		"--expect-current-kind", "missing", "--expect-current-model", "gpt-5.6-terra",
	})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "single") {
		t.Fatalf("error = %v, want invalid kind", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}
