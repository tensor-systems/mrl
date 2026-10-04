package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testKeysProjectID = "33333333-3333-4333-8333-333333333333"
	testKeysOtherID   = "44444444-4444-4444-8444-444444444444"
	testKeysKeyID     = "55555555-5555-4555-8555-555555555555"
	testKeysSecret    = "mr_sk_test_secret_value"
)

// keysTestServer fakes the account endpoints 'mrl keys create' uses. meProject
// is the default project /auth/me reports ("" for none); projects is the
// /projects listing. It records the project_id each created key asked for.
type keysTestServer struct {
	t           *testing.T
	token       string
	meProject   string
	projects    string
	createdFor  []string
	refreshHits int
}

func (s *keysTestServer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/auth/refresh" {
		s.refreshHits++
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "good-refresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.token = "fresh-token"
		_, _ = w.Write([]byte(`{"access_token":"fresh-token","refresh_token":"rotated-refresh"}`))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/auth/me":
		if s.meProject == "" {
			_, _ = w.Write([]byte(`{"user":{"id":"` + testKeysOtherID + `"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"user":{"project_id":"` + s.meProject + `"}}`))
	case r.Method == http.MethodGet && r.URL.Path == "/projects":
		_, _ = w.Write([]byte(s.projects))
	case r.Method == http.MethodPost && r.URL.Path == "/api-keys":
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Errorf("decode create body: %v", err)
		}
		if body["label"] != "hark" || body["kind"] != "secret" {
			s.t.Errorf("create body = %#v", body)
		}
		s.createdFor = append(s.createdFor, body["project_id"])
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"api_key":{"id":"` + testKeysKeyID + `","project_id":"` + body["project_id"] +
			`","label":"hark","kind":"secret","created_at":"2026-10-04T00:00:00Z","redacted_key":"mr_sk_te…alue","secret_key":"` +
			testKeysSecret + `"}}`))
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func runKeysCreate(t *testing.T, cfg runtimeConfig, args ...string) (string, string, error) {
	t.Helper()
	command := newKeysCreateCmd()
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetContext(withRuntimeConfig(t.Context(), cfg))
	command.SetArgs(args)
	err := command.Execute()
	return stdout.String(), stderr.String(), err
}

func useTempConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestKeysCreate_PrintWritesOnlySecretAndSavesNothing(t *testing.T) {
	useTempConfig(t)
	fake := &keysTestServer{t: t, token: "account-token", meProject: testKeysProjectID}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()

	stdout, stderr, err := runKeysCreate(t, runtimeConfig{
		Profile: "default", BaseURL: server.URL, Token: "account-token", Timeout: time.Second,
	}, "--name", "hark", "--print")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if stdout != testKeysSecret+"\n" {
		t.Fatalf("stdout = %q, want only the secret", stdout)
	}
	if !strings.Contains(stderr, testKeysKeyID) {
		t.Fatalf("stderr = %q, want a diagnostic naming the key", stderr)
	}
	cfg, err := loadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := profileFor(cfg, "default").APIKey; got != "" {
		t.Fatalf("--print saved the key to the profile: %q", got)
	}
}

func TestKeysCreate_DefaultSavesKeyToProfile(t *testing.T) {
	useTempConfig(t)
	fake := &keysTestServer{t: t, token: "account-token", meProject: testKeysProjectID}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()

	stdout, _, err := runKeysCreate(t, runtimeConfig{
		Profile: "work", BaseURL: server.URL, Token: "account-token", Timeout: time.Second,
	}, "--name", "hark")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{testKeysKeyID, "mr_sk_te…alue", testKeysProjectID, testKeysSecret} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	cfg, err := loadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := profileFor(cfg, "work").APIKey; got != testKeysSecret {
		t.Fatalf("profile api_key = %q, want the new secret", got)
	}
}

func TestKeysCreate_UsesAccountDefaultProject(t *testing.T) {
	useTempConfig(t)
	fake := &keysTestServer{t: t, token: "account-token", meProject: testKeysProjectID}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()

	if _, _, err := runKeysCreate(t, runtimeConfig{
		Profile: "default", BaseURL: server.URL, Token: "account-token", Timeout: time.Second,
	}, "--name", "hark", "--print"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(fake.createdFor) != 1 || fake.createdFor[0] != testKeysProjectID {
		t.Fatalf("created for %v, want the default project", fake.createdFor)
	}
}

func TestKeysCreate_ExplicitProjectSkipsLookup(t *testing.T) {
	useTempConfig(t)
	fake := &keysTestServer{t: t, token: "account-token"}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()

	if _, _, err := runKeysCreate(t, runtimeConfig{
		Profile: "default", BaseURL: server.URL, Token: "account-token", ProjectID: testKeysOtherID, Timeout: time.Second,
	}, "--name", "hark", "--print"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(fake.createdFor) != 1 || fake.createdFor[0] != testKeysOtherID {
		t.Fatalf("created for %v, want the explicit project", fake.createdFor)
	}
}

func TestKeysCreate_SoleProjectWhenNoDefault(t *testing.T) {
	useTempConfig(t)
	fake := &keysTestServer{t: t, token: "account-token",
		projects: `{"projects":[{"id":"` + testKeysOtherID + `","name":"Only"}]}`}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()

	if _, _, err := runKeysCreate(t, runtimeConfig{
		Profile: "default", BaseURL: server.URL, Token: "account-token", Timeout: time.Second,
	}, "--name", "hark", "--print"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(fake.createdFor) != 1 || fake.createdFor[0] != testKeysOtherID {
		t.Fatalf("created for %v, want the sole project", fake.createdFor)
	}
}

func TestKeysCreate_SeveralProjectsNoDefaultListsThem(t *testing.T) {
	useTempConfig(t)
	fake := &keysTestServer{t: t, token: "account-token",
		projects: `{"projects":[{"id":"` + testKeysProjectID + `","name":"Alpha"},{"id":"` + testKeysOtherID + `","name":"Beta"}]}`}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()

	stdout, _, err := runKeysCreate(t, runtimeConfig{
		Profile: "default", BaseURL: server.URL, Token: "account-token", Timeout: time.Second,
	}, "--name", "hark", "--print")
	if err == nil {
		t.Fatal("expected an error when several projects and no default")
	}
	for _, want := range []string{"--project", testKeysProjectID, "Alpha", testKeysOtherID, "Beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if stdout != "" || len(fake.createdFor) != 0 {
		t.Fatalf("no key should be created; stdout=%q created=%v", stdout, fake.createdFor)
	}
}

func TestKeysCreate_MissingLogin(t *testing.T) {
	useTempConfig(t)
	stdout, _, err := runKeysCreate(t, runtimeConfig{
		Profile: "default", BaseURL: "http://127.0.0.1:1", Timeout: time.Second,
	}, "--name", "hark", "--print")
	if !errors.Is(err, errAccountLoginRequired) {
		t.Fatalf("err = %v, want login-required", err)
	}
	if !strings.Contains(err.Error(), "mrl auth login --web") {
		t.Fatalf("error should tell the user to log in: %v", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
}

func TestKeysCreate_RefreshesExpiredToken(t *testing.T) {
	useTempConfig(t)
	fake := &keysTestServer{t: t, token: "current-token", meProject: testKeysProjectID}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()

	stdout, _, err := runKeysCreate(t, runtimeConfig{
		Profile: "default", BaseURL: server.URL, Token: "expired-token", RefreshToken: "good-refresh", Timeout: time.Second,
	}, "--name", "hark", "--print")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if stdout != testKeysSecret+"\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if fake.refreshHits != 1 {
		t.Fatalf("refresh hits = %d, want 1", fake.refreshHits)
	}
	cfg, err := loadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	profile := profileFor(cfg, "default")
	if profile.Token != "fresh-token" || profile.RefreshToken != "rotated-refresh" {
		t.Fatalf("refreshed tokens not persisted: %+v", profile)
	}
}

func TestKeysCreate_ExpiredTokenWithoutUsableRefresh(t *testing.T) {
	useTempConfig(t)
	fake := &keysTestServer{t: t, token: "current-token", meProject: testKeysProjectID}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()

	_, _, err := runKeysCreate(t, runtimeConfig{
		Profile: "default", BaseURL: server.URL, Token: "expired-token", RefreshToken: "revoked-refresh", Timeout: time.Second,
	}, "--name", "hark", "--print")
	if !errors.Is(err, errAccountLoginRequired) {
		t.Fatalf("err = %v, want login-required", err)
	}
	if len(fake.createdFor) != 0 {
		t.Fatalf("no key should be created: %v", fake.createdFor)
	}
}
