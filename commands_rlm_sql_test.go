package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestBuildLocalSQLDataSource_PostgresUsesBrokerWithoutDSN(t *testing.T) {
	flags := &rlmFlags{postgresDSNEnv: "APP_DATABASE_URL", dbName: "warehouse"}
	cfg := runtimeConfig{APIKey: "mr_sk_test"}
	server := testLocalServer()
	server.BrokerURL = "http://127.0.0.1:1/sql/source"

	specs, defaultSource, err := buildLocalSQLDataSource(flags, cfg, server)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 1 || defaultSource != "warehouse" {
		t.Fatalf("unexpected source: %#v / %q", specs, defaultSource)
	}
	spec := specs[0]
	if spec.BrokerURL != server.BrokerURL || spec.BrokerToken != server.Token {
		t.Fatalf("broker wiring mismatch: %+v", spec)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("APP_DATABASE_URL")) || bytes.Contains(encoded, []byte("postgres://")) {
		t.Fatalf("runner spec leaked PostgreSQL configuration: %s", encoded)
	}
}

func TestBuildLocalSQLDataSource_RejectsTwoDatabases(t *testing.T) {
	flags := &rlmFlags{db: "app.db", postgresDSNEnv: "APP_DATABASE_URL"}
	_, _, err := buildLocalSQLDataSource(flags, runtimeConfig{APIKey: "mr_sk_test"}, testLocalServer())
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestBuildLocalSQLDataSource_SnowflakeUsesOnlyBrokerCapability(t *testing.T) {
	const tokenEnv = "MODELRELAY_TEST_SNOWFLAKE_BROKER_TOKEN"
	token := strings.Repeat("s", 32)
	t.Setenv(tokenEnv, token)
	flags := &rlmFlags{
		snowflakeBrokerURL:      "http://snowflake-edge:8787/v1/sql",
		snowflakeBrokerTokenEnv: tokenEnv,
		dbName:                  "warehouse",
	}
	specs, defaultSource, err := buildLocalSQLDataSource(flags, runtimeConfig{}, testLocalServer())
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || defaultSource != "warehouse" || specs[0].BrokerURL != flags.snowflakeBrokerURL || specs[0].BrokerToken != token {
		t.Fatalf("unexpected Snowflake broker source: %#v / %q", specs, defaultSource)
	}
	encoded, err := json.Marshal(specs[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(tokenEnv)) || bytes.Contains(encoded, []byte("private_key")) || bytes.Contains(encoded, []byte("oauth")) {
		t.Fatalf("runner spec leaked Snowflake configuration: %s", encoded)
	}
}

func TestBuildLocalSQLDataSource_SnowflakeRejectsCallerPolicyAndAmbiguousSources(t *testing.T) {
	const tokenEnv = "MODELRELAY_TEST_SNOWFLAKE_BROKER_TOKEN"
	t.Setenv(tokenEnv, strings.Repeat("s", 32))
	flags := &rlmFlags{
		snowflakeBrokerURL: "http://snowflake-edge:8787/v1/sql", snowflakeBrokerTokenEnv: tokenEnv,
		sqlProfile: "caller-controlled",
	}
	if _, _, err := buildLocalSQLDataSource(flags, runtimeConfig{}, testLocalServer()); err == nil || !strings.Contains(err.Error(), "trusted broker owns") {
		t.Fatalf("expected caller policy rejection, got %v", err)
	}
	flags.sqlProfile = ""
	flags.db = "app.db"
	if _, _, err := buildLocalSQLDataSource(flags, runtimeConfig{}, testLocalServer()); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected source ambiguity rejection, got %v", err)
	}
}

func TestBrokerURLWithRunIDPreservesPath(t *testing.T) {
	got, err := brokerURLWithRunID("https://edge.example.test/v1/sql", "run/one")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://edge.example.test/v1/sql?run_id=run%2Fone" {
		t.Fatalf("unexpected tagged broker URL: %q", got)
	}
}

func TestOpenLocalPostgresConnector_RequiresNamedEnvironmentValue(t *testing.T) {
	const envName = "MODELRELAY_TEST_MISSING_POSTGRES_DSN"
	t.Setenv(envName, "")
	_, err := openLocalPostgresConnector(context.Background(), &rlmFlags{postgresDSNEnv: envName}, runtimeConfig{APIKey: "mr_sk_test"})
	if err == nil || !strings.Contains(err.Error(), "unset or empty") {
		t.Fatalf("expected missing environment error, got %v", err)
	}
}

func TestPostgresRunnerEnvironmentRemovesDSN(t *testing.T) {
	environment := []string{
		"PATH=/usr/bin",
		"MODELRELAY_POSTGRES_DSN=postgres://reader:secret@db/analytics",
		"MODELRELAY_POSTGRES_DSN_BACKUP=keep",
	}
	filtered := environmentWithoutVariable(environment, "MODELRELAY_POSTGRES_DSN")
	joined := strings.Join(filtered, "\n")
	if strings.Contains(joined, "reader:secret") {
		t.Fatalf("DSN leaked into runner environment: %q", joined)
	}
	if !strings.Contains(joined, "MODELRELAY_POSTGRES_DSN_BACKUP=keep") || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatalf("unrelated environment variables were removed: %q", joined)
	}
}

func TestNewLocalPostgresBrokerConfig_NilConnectorDisablesBroker(t *testing.T) {
	if config := newLocalPostgresBrokerConfig(&rlmFlags{}, nil); config != nil {
		t.Fatalf("expected no broker config, got %#v", config)
	}
}

type fakePostgresSQLSource struct {
	gotSQL       string
	gotTimeoutMS int
	rows         []map[string]any
	schema       string
	queryErr     error
}

func (f *fakePostgresSQLSource) Query(_ context.Context, sql string, timeoutMS int) ([]map[string]any, error) {
	f.gotSQL = sql
	f.gotTimeoutMS = timeoutMS
	return f.rows, f.queryErr
}

func (f *fakePostgresSQLSource) Schema(context.Context) (string, error) { return f.schema, nil }

func TestLocalPostgresBroker_ValidatesThenExecutesNormalizedSQL(t *testing.T) {
	var validation map[string]any
	validator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-ModelRelay-Api-Key"); got != "mr_sk_test" {
			t.Errorf("unexpected API key: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&validation); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"valid": true, "normalized_sql": "SELECT name FROM users LIMIT 1", "timeout_ms": 321,
		})
	}))
	defer validator.Close()

	source := &fakePostgresSQLSource{rows: []map[string]any{{"name": "ada"}}}
	handler := &localPostgresBrokerHandler{
		baseURL: validator.URL, apiKey: "mr_sk_test", token: "tok", source: source,
		policy: defaultLocalPostgresPolicy,
	}
	req := httptest.NewRequest(http.MethodPost, "/sql/source", strings.NewReader(`{"method":"query","sql":"SELECT name FROM users"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if source.gotSQL != "SELECT name FROM users LIMIT 1" || source.gotTimeoutMS != 321 {
		t.Fatalf("broker did not execute validated output: %q / %d", source.gotSQL, source.gotTimeoutMS)
	}
	policy, ok := validation["policy"].(map[string]any)
	if !ok || policy["dialect"] != "postgres" || policy["read_only"] != true {
		t.Fatalf("host-owned PostgreSQL policy missing: %#v", validation)
	}
	if !strings.Contains(rec.Body.String(), `"name":"ada"`) {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
}

func TestLocalPostgresBroker_DoesNotExecuteRejectedSQL(t *testing.T) {
	validator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"only SELECT statements are allowed"}}`))
	}))
	defer validator.Close()

	source := &fakePostgresSQLSource{}
	handler := &localPostgresBrokerHandler{baseURL: validator.URL, apiKey: "key", token: "tok", source: source, policy: defaultLocalPostgresPolicy}
	req := httptest.NewRequest(http.MethodPost, "/sql/source", strings.NewReader(`{"method":"query","sql":"DELETE FROM users"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "only SELECT") {
		t.Fatalf("expected policy rejection, got %d: %s", rec.Code, rec.Body.String())
	}
	if source.gotSQL != "" {
		t.Fatalf("rejected SQL reached connector: %q", source.gotSQL)
	}
}

func TestLocalPostgresBroker_ExecutionFailureIsNotPolicyRejection(t *testing.T) {
	validator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"valid": true, "normalized_sql": "SELECT value FROM large_table LIMIT 1000", "timeout_ms": 5000,
		})
	}))
	defer validator.Close()

	source := &fakePostgresSQLSource{queryErr: errors.New("query exceeded the local response limit (1048576 bytes)")}
	handler := &localPostgresBrokerHandler{baseURL: validator.URL, apiKey: "key", token: "tok", source: source, policy: defaultLocalPostgresPolicy}
	req := httptest.NewRequest(http.MethodPost, "/sql/source", strings.NewReader(`{"method":"query","sql":"SELECT value FROM large_table"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for execution failure, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "response limit") {
		t.Fatalf("expected execution failure detail, got %s", rec.Body.String())
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
