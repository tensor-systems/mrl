package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelrelay/modelrelay/sdk/go"
)

func TestExecuteRLMRemote_Progress(t *testing.T) {
	var (
		gotPath   string
		gotKey    string
		gotClient string
		gotModel  string
		gotQuery  string
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

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"demo","answer":"ok","iterations":1,"subcalls":1,"usage":{},"trajectory":[],"progress":[{"status":"step 1"}]}`))
	}))
	t.Cleanup(server.Close)

	req := rlmExecuteRemoteRequest{Model: "demo", Query: "hi"}
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
