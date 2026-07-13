package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelrelay/modelrelay/sdk/go"
)

// capturedResponsesServer emulates /responses and records request bodies so
// tests can assert what the local subcall proxy forwards (rlm-core#25).
type capturedResponsesServer struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (c *capturedResponsesServer) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{
		"id": "resp-1",
		"output": [{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}],
		"model": "echo-1",
		"usage": {"input_tokens":1,"output_tokens":1,"total_tokens":2}
	}`))
}

func (c *capturedResponsesServer) last(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatalf("no /responses request captured")
	}
	return c.bodies[len(c.bodies)-1]
}

func newSubcallTestHandler(t *testing.T, captured *capturedResponsesServer, defaults localSubcallDefaults) (*localSubcallHandler, func()) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(captured.handler))

	client, err := sdk.NewClientWithKey(mustParseAPIKey(t), sdk.WithBaseURL(upstream.URL))
	if err != nil {
		upstream.Close()
		t.Fatalf("client: %v", err)
	}
	counter := 0
	handler := &localSubcallHandler{
		ctx:             context.Background(),
		client:          client,
		defaultModel:    "echo-1",
		subcallDefaults: defaults,
		maxDepth:        1,
		maxSubcalls:     10,
		token:           "test-token",
		counter:         &counter,
		usage:           &rlmUsage{},
	}
	return handler, upstream.Close
}

func mustParseAPIKey(t *testing.T) sdk.APIKeyAuth {
	t.Helper()
	key, err := sdk.ParseAPIKeyAuth("mr_sk_testtesttesttesttesttest")
	if err != nil {
		t.Fatalf("parse api key: %v", err)
	}
	return key
}

func doLocalSubcall(t *testing.T, handler *localSubcallHandler, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/rlm/subcall", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestLocalSubcallHandler_AppliesCostDefaults(t *testing.T) {
	captured := &capturedResponsesServer{}
	handler, cleanup := newSubcallTestHandler(t, captured, localSubcallDefaults{})
	defer cleanup()

	rec := doLocalSubcall(t, handler, map[string]any{"prompt": "hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body := captured.last(t)
	if got := body["max_output_tokens"]; got != float64(localDefaultSubcallMaxOutputTokens) {
		t.Fatalf("max_output_tokens = %v, want %d", got, localDefaultSubcallMaxOutputTokens)
	}
	if got := body["reasoning_effort"]; got != localDefaultSubcallReasoningEffort {
		t.Fatalf("reasoning_effort = %v, want %q", got, localDefaultSubcallReasoningEffort)
	}
	if got := body["model"]; got != "echo-1" {
		t.Fatalf("model = %v, want echo-1", got)
	}
}

func TestLocalSubcallHandler_FlagDefaultsAndPayloadOverrides(t *testing.T) {
	captured := &capturedResponsesServer{}
	handler, cleanup := newSubcallTestHandler(t, captured, localSubcallDefaults{
		MaxOutputTokens: 4096,
		Model:           "grok-4-fast-non-reasoning",
		ReasoningEffort: "low",
	})
	defer cleanup()

	// Flag values apply when the payload has no explicit values.
	rec := doLocalSubcall(t, handler, map[string]any{"prompt": "hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := captured.last(t)
	if got := body["max_output_tokens"]; got != float64(4096) {
		t.Fatalf("max_output_tokens = %v, want 4096", got)
	}
	if got := body["reasoning_effort"]; got != "low" {
		t.Fatalf("reasoning_effort = %v, want low", got)
	}
	if got := body["model"]; got != "grok-4-fast-non-reasoning" {
		t.Fatalf("model = %v, want grok-4-fast-non-reasoning", got)
	}

	// Per-payload values win over flag values.
	rec = doLocalSubcall(t, handler, map[string]any{
		"prompt":            "hello",
		"model":             "echo-2",
		"max_output_tokens": 128,
		"reasoning_effort":  "high",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body = captured.last(t)
	if got := body["max_output_tokens"]; got != float64(128) {
		t.Fatalf("max_output_tokens = %v, want 128", got)
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got)
	}
	if got := body["model"]; got != "echo-2" {
		t.Fatalf("model = %v, want echo-2", got)
	}
}

func TestLocalSubcallHandler_RejectsInvalidCostControls(t *testing.T) {
	captured := &capturedResponsesServer{}
	handler, cleanup := newSubcallTestHandler(t, captured, localSubcallDefaults{})
	defer cleanup()

	rec := doLocalSubcall(t, handler, map[string]any{"prompt": "hello", "reasoning_effort": "max"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reasoning_effort") {
		t.Fatalf("body = %s, want reasoning_effort error", rec.Body.String())
	}

	rec = doLocalSubcall(t, handler, map[string]any{"prompt": "hello", "max_output_tokens": 0})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestValidSubcallReasoningEffort_AcceptsFullLadderAndRejectsJunk(t *testing.T) {
	for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh"} {
		if !validSubcallReasoningEffort(effort) {
			t.Errorf("validSubcallReasoningEffort(%q) = false, want true", effort)
		}
	}
	for _, effort := range []string{"max", "MEDIUM", "ultra", "pro"} {
		if validSubcallReasoningEffort(effort) {
			t.Errorf("validSubcallReasoningEffort(%q) = true, want false", effort)
		}
	}
}
