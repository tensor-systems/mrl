package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelrelay/modelrelay/platform/rlm"
	sdk "github.com/modelrelay/modelrelay/sdk/go"
)

func TestRLMExecuteRemoteRequest_HasNoLegacyMaxIterationsControl(t *testing.T) {
	payload, err := json.Marshal(rlmExecuteRemoteRequest{Model: "demo", Query: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("max_iterations")) {
		t.Fatalf("remote request leaked removed control: %s", payload)
	}
	if !bytes.Contains(payload, []byte(`"seed":null`)) {
		t.Fatalf("remote request omitted explicit unavailable seed: %s", payload)
	}
}

func TestRunRLMRemote_RejectsLocalExecutionTimeout(t *testing.T) {
	err := runRLMRemote(
		context.Background(), runtimeConfig{}, sdk.SecretKey("mr_sk_test"), "demo", "hi",
		nil, rlm.ContextPlan{}, &rlmFlags{execTimeoutMS: 1000}, false,
	)
	if err == nil || !strings.Contains(err.Error(), "local-mode only") {
		t.Fatalf("error = %v, want local-only timeout rejection", err)
	}
}

func TestExecuteRLMRemote_Progress(t *testing.T) {
	var (
		gotPath   string
		gotKey    string
		gotClient string
		gotModel  string
		gotQuery  string
		gotSeed   *int64
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-ModelRelay-Api-Key")
		gotClient = r.Header.Get("X-ModelRelay-Client")

		var req rlmExecuteRemoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotModel = req.Model
		gotQuery = req.Query
		gotSeed = req.Seed

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"demo","answer":"ok","iterations":1,"subcalls":1,"usage":{},"trajectory":[],"progress":[{"status":"step 1"}]}`))
	}))
	t.Cleanup(server.Close)

	seed := int64(42)
	req := rlmExecuteRemoteRequest{Model: "demo", Query: "hi", Seed: &seed}
	result, err := executeRLMRemote(context.Background(), server.Client(), server.URL, sdk.SecretKey("mr_sk_test"), req)
	if err != nil {
		t.Fatalf("executeRLMRemote error: %v", err)
	}

	if gotPath != "/rlm/execute" {
		t.Fatalf("path = %q, want %q", gotPath, "/rlm/execute")
	}
	if gotKey != "mr_sk_test" {
		t.Fatalf("api key = %q, want %q", gotKey, "mr_sk_test")
	}
	if gotClient == "" {
		t.Fatalf("expected X-ModelRelay-Client header")
	}
	if gotModel != "demo" || gotQuery != "hi" {
		t.Fatalf("request model/query = %q/%q, want demo/hi", gotModel, gotQuery)
	}
	if gotSeed == nil || *gotSeed != 42 {
		t.Fatalf("request seed = %v, want 42", gotSeed)
	}
	if len(result.Progress) != 1 || result.Progress[0].Status != "step 1" {
		t.Fatalf("progress = %+v, want status 'step 1'", result.Progress)
	}
}

func TestValidateRLMRemoteAttachments_RejectsMissingText(t *testing.T) {
	err := validateRLMRemoteAttachments([]rlmFileAttachment{{Name: "data.csv"}})
	if err == nil {
		t.Fatalf("expected error for missing inline text")
	}
}
