package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelrelay/modelrelay/sdk/go"
	"github.com/modelrelay/modelrelay/sdk/go/llm"
)

// newSlowResponsesServer returns a test server whose /responses handler waits
// for delay before answering with a minimal valid completion.
func newSlowResponsesServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.Response{
			ID:    "resp_test",
			Model: "demo",
			Output: []llm.OutputItem{{
				Type:    llm.OutputItemTypeMessage,
				Role:    llm.RoleAssistant,
				Content: []llm.ContentPart{llm.TextPart("ok")},
			}},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// promptOnce issues a single non-streaming completion through the client that
// newPromptClient builds from cfg, using a background context so the only
// deadline in play is the SDK's per-request timeout.
func promptOnce(t *testing.T, cfg runtimeConfig) error {
	t.Helper()
	client, err := newPromptClient(cfg)
	if err != nil {
		t.Fatalf("newPromptClient: %v", err)
	}
	req, opts, err := client.Responses.New().Model(sdk.NewModelID("demo")).User("ping").Build()
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	_, err = client.Responses.Create(context.Background(), req, opts...)
	return err
}

func TestNewPromptClient_TimeoutBelowServerDelay_RequestFailsAtConfiguredTimeout(t *testing.T) {
	server := newSlowResponsesServer(t, 500*time.Millisecond)
	cfg := runtimeConfig{BaseURL: server.URL, APIKey: "mr_sk_test", Timeout: 100 * time.Millisecond}

	start := time.Now()
	err := promptOnce(t, cfg)
	elapsed := time.Since(start)

	// Before the fix the SDK's 60s per-request default applied regardless of
	// --timeout, so this request would have succeeded after ~500ms.
	if err == nil {
		t.Fatal("expected request to fail at the configured 100ms timeout, got success")
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("request outlived the configured timeout (elapsed %s); --timeout was not applied per-request", elapsed)
	}
}

func TestNewPromptClient_TimeoutAboveServerDelay_RequestSucceeds(t *testing.T) {
	server := newSlowResponsesServer(t, 200*time.Millisecond)
	cfg := runtimeConfig{BaseURL: server.URL, APIKey: "mr_sk_test", Timeout: 5 * time.Second}

	if err := promptOnce(t, cfg); err != nil {
		t.Fatalf("expected success with --timeout above the server delay, got %v", err)
	}
}

func TestNewPromptClient_NoTimeoutConfigured_SDKDefaultUnchanged(t *testing.T) {
	server := newSlowResponsesServer(t, 200*time.Millisecond)
	cfg := runtimeConfig{BaseURL: server.URL, APIKey: "mr_sk_test"} // Timeout zero: option must not be sent

	// With no configured timeout the SDK's own default (60s) governs; a slow
	// but sub-default request must still succeed (in particular the zero value
	// must not be forwarded, which would disable/zero the request timeout).
	if err := promptOnce(t, cfg); err != nil {
		t.Fatalf("expected success under the SDK default request timeout, got %v", err)
	}
}
