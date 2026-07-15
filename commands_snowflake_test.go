package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/modelrelay/modelrelay/platform/snowflakesource"
	"github.com/modelrelay/modelrelay/platform/sqlprofiles"
)

type fakeSnowflakeSource struct {
	querySQL      string
	queryMetadata snowflakesource.QueryMetadata
}

func (f *fakeSnowflakeSource) Ping(context.Context) error { return nil }
func (f *fakeSnowflakeSource) TestConnection(context.Context) (snowflakesource.ConnectionState, error) {
	return snowflakesource.ConnectionState{}, nil
}
func (f *fakeSnowflakeSource) Schema(context.Context, snowflakesource.QueryMetadata) (string, error) {
	return "safe schema", nil
}
func (f *fakeSnowflakeSource) Query(_ context.Context, sql string, _ int, metadata snowflakesource.QueryMetadata) ([]map[string]any, error) {
	f.querySQL, f.queryMetadata = sql, metadata
	return []map[string]any{{"NAME": "Ada"}}, nil
}

func testSnowflakePolicy() sqlprofiles.Policy {
	readOnly := true
	return sqlprofiles.Policy{
		Dialect: sqlprofiles.Dialect("snowflake"), ReadOnly: &readOnly,
		Limits:     sqlprofiles.Limits{DefaultLimit: 100, MaxLimit: 100, TimeoutMS: 1000},
		Subqueries: sqlprofiles.SubqueryPolicy{Allowed: true},
		Aggregations: sqlprofiles.AggregationPolicy{
			Allowed: true, Functions: []string{"count", "sum", "avg", "min", "max"},
		},
	}
}

func TestSnowflakeBrokerRequiresAuthentication(t *testing.T) {
	source := &fakeSnowflakeSource{}
	handler := &snowflakeBrokerHandler{token: strings.Repeat("t", 32), config: snowflakeEdgeConfig{Policy: testSnowflakePolicy(), TenantID: "tenant-a"}, source: source}
	request := httptest.NewRequest(http.MethodPost, "/v1/sql", strings.NewReader(`{"method":"schema"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestSnowflakeBrokerValidatesAndTagsQuery(t *testing.T) {
	source := &fakeSnowflakeSource{}
	token := strings.Repeat("t", 32)
	handler := &snowflakeBrokerHandler{token: token, config: snowflakeEdgeConfig{Policy: testSnowflakePolicy(), TenantID: "tenant-a", SourceID: "warehouse-a"}, source: source}
	request := httptest.NewRequest(http.MethodPost, "/v1/sql?run_id=run-a", strings.NewReader(`{"method":"query","sql":"SELECT NAME FROM USERS"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if source.querySQL == "" || !strings.Contains(strings.ToUpper(source.querySQL), "LIMIT 100") {
		t.Fatalf("query was not locally normalized and bounded: %q", source.querySQL)
	}
	if source.queryMetadata.RunID != "run-a" || source.queryMetadata.TenantID != "tenant-a" || source.queryMetadata.Source != "warehouse-a" {
		t.Fatalf("unexpected metadata: %#v", source.queryMetadata)
	}
}

func TestSnowflakeBrokerRejectsMutationBeforeSource(t *testing.T) {
	source := &fakeSnowflakeSource{}
	token := strings.Repeat("t", 32)
	handler := &snowflakeBrokerHandler{token: token, config: snowflakeEdgeConfig{Policy: testSnowflakePolicy(), TenantID: "tenant-a"}, source: source}
	request := httptest.NewRequest(http.MethodPost, "/v1/sql", strings.NewReader(`{"method":"query","sql":"DELETE FROM USERS"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || source.querySQL != "" {
		t.Fatalf("mutation reached source: status=%d sql=%q", recorder.Code, source.querySQL)
	}
}

func TestSnowflakeHealthAndReadinessAreSeparated(t *testing.T) {
	server := newSnowflakeHTTPServer("127.0.0.1:0", strings.Repeat("t", 32), snowflakeEdgeConfig{}, &fakeSnowflakeSource{})
	health := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthRecorder := httptest.NewRecorder()
	server.Handler.ServeHTTP(healthRecorder, health)
	if healthRecorder.Code != http.StatusOK {
		t.Fatalf("health status = %d", healthRecorder.Code)
	}
	ready := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	readyRecorder := httptest.NewRecorder()
	server.Handler.ServeHTTP(readyRecorder, ready)
	if readyRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated readiness status = %d", readyRecorder.Code)
	}
}

func TestSnowflakeConfigJSONCannotContainPassword(t *testing.T) {
	path := writeTempSnowflakeConfig(t, `{
  "snowflake":{"account":"org-acct","warehouse":"W","database":"D","schema":"S","role":"R","password":"secret","auth":{"type":"workload_identity","workload_identity":{"provider":"AWS"}}},
  "tenant_id":"tenant-a","policy":{"dialect":"snowflake","read_only":true}
}`)
	if _, err := loadSnowflakeEdgeConfig(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected password field rejection, got %v", err)
	}
}

func writeTempSnowflakeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := t.TempDir() + "/snowflake.json"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
