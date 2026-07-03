package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLocalServer() localRLMServer {
	return localRLMServer{
		ValidateURL: "http://127.0.0.1:1/sql/validate",
		Token:       "tok",
	}
}

func TestBuildLocalSQLDataSource_NoDBFlag_ReturnsNothing(t *testing.T) {
	specs, defaultSource, err := buildLocalSQLDataSource(&rlmFlags{}, runtimeConfig{}, testLocalServer())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if specs != nil || defaultSource != "" {
		t.Fatalf("expected no data sources, got %v / %q", specs, defaultSource)
	}
}

func TestBuildLocalSQLDataSource_RequiresAPIKey(t *testing.T) {
	flags := &rlmFlags{db: "some.db"}
	_, _, err := buildLocalSQLDataSource(flags, runtimeConfig{}, testLocalServer())
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("expected API key error, got %v", err)
	}
}

func TestBuildLocalSQLDataSource_MissingFile_Errors(t *testing.T) {
	flags := &rlmFlags{db: filepath.Join(t.TempDir(), "missing.db")}
	cfg := runtimeConfig{APIKey: "mr_sk_test"}
	_, _, err := buildLocalSQLDataSource(flags, cfg, testLocalServer())
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestBuildLocalSQLDataSource_RejectsDirectory(t *testing.T) {
	flags := &rlmFlags{db: t.TempDir()}
	cfg := runtimeConfig{APIKey: "mr_sk_test"}
	_, _, err := buildLocalSQLDataSource(flags, cfg, testLocalServer())
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error, got %v", err)
	}
}

func TestBuildLocalSQLDataSource_DefaultPolicy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.db")
	if err := os.WriteFile(dbPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := &rlmFlags{db: dbPath, dbName: "db"}
	cfg := runtimeConfig{APIKey: "mr_sk_test"}
	server := testLocalServer()

	specs, defaultSource, err := buildLocalSQLDataSource(flags, cfg, server)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	spec := specs[0]
	if spec.Type != "sql" || spec.Name != "db" || defaultSource != "db" {
		t.Fatalf("unexpected spec identity: %+v default=%q", spec, defaultSource)
	}
	if spec.SQLitePath != dbPath {
		t.Fatalf("expected absolute sqlite path %q, got %q", dbPath, spec.SQLitePath)
	}
	if spec.ValidateURL != server.ValidateURL || spec.ValidateToken != server.Token {
		t.Fatalf("validate wiring mismatch: %+v", spec)
	}
	if spec.ProfileID != "" {
		t.Fatalf("expected no profile, got %q", spec.ProfileID)
	}
	var policy map[string]any
	if err := json.Unmarshal(spec.Policy, &policy); err != nil {
		t.Fatalf("default policy is not valid JSON: %v", err)
	}
	if policy["read_only"] != true {
		t.Fatalf("default policy must be read-only: %v", policy)
	}
	aggs, ok := policy["aggregations"].(map[string]any)
	if !ok || aggs["allowed"] != true {
		t.Fatalf("default policy should allow aggregations: %v", policy)
	}
}

func TestBuildLocalSQLDataSource_ProfileOverridesPolicy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app.db")
	if err := os.WriteFile(dbPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := &rlmFlags{db: dbPath, dbName: "analytics", sqlProfile: "9f0d8f9e-0000-0000-0000-000000000000"}
	cfg := runtimeConfig{APIKey: "mr_sk_test"}

	specs, defaultSource, err := buildLocalSQLDataSource(flags, cfg, testLocalServer())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spec := specs[0]
	if spec.ProfileID != flags.sqlProfile {
		t.Fatalf("expected profile %q, got %q", flags.sqlProfile, spec.ProfileID)
	}
	if len(spec.Policy) != 0 {
		t.Fatalf("profile and inline policy must be mutually exclusive, got policy %s", spec.Policy)
	}
	if defaultSource != "analytics" {
		t.Fatalf("expected default source analytics, got %q", defaultSource)
	}
}

func TestLocalSQLValidateHandler_RejectsBadToken(t *testing.T) {
	handler := &localSQLValidateHandler{ctx: context.Background(), baseURL: "http://unused", token: "right"}
	req := httptest.NewRequest(http.MethodPost, "/sql/validate", strings.NewReader(`{"sql":"SELECT 1"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestLocalSQLValidateHandler_ForwardsWithAPIKey(t *testing.T) {
	var gotBody []byte
	var gotAPIKey string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-ModelRelay-Api-Key")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"valid":true,"normalized_sql":"SELECT 1 LIMIT 10"}`))
	}))
	defer backend.Close()

	handler := &localSQLValidateHandler{
		ctx:     context.Background(),
		baseURL: backend.URL,
		apiKey:  "mr_sk_test",
		token:   "tok",
	}
	body := `{"sql":"SELECT 1","policy":{"read_only":true}}`
	req := httptest.NewRequest(http.MethodPost, "/sql/validate", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if gotAPIKey != "mr_sk_test" {
		t.Fatalf("expected API key forwarded, got %q", gotAPIKey)
	}
	if !bytes.Equal(gotBody, []byte(body)) {
		t.Fatalf("body not forwarded verbatim: %s", gotBody)
	}
	if !strings.Contains(rec.Body.String(), "normalized_sql") {
		t.Fatalf("backend response not copied: %s", rec.Body.String())
	}
}

func TestLocalSQLValidateHandler_CopiesErrorStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"only SELECT statements are allowed"}}`))
	}))
	defer backend.Close()

	handler := &localSQLValidateHandler{ctx: context.Background(), baseURL: backend.URL, apiKey: "k", token: "tok"}
	req := httptest.NewRequest(http.MethodPost, "/sql/validate", strings.NewReader(`{"sql":"DELETE FROM x"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 copied through, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "only SELECT") {
		t.Fatalf("validator message not copied: %s", rec.Body.String())
	}
}
