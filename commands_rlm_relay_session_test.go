package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelrelay/modelrelay/platform/pricing"
	"github.com/modelrelay/modelrelay/platform/rlm"
	"github.com/modelrelay/modelrelay/platform/rlmprofile"
	sdk "github.com/modelrelay/modelrelay/sdk/go"
)

const relaySessionScaffoldID = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

var relayPreflightRequestPathPattern = regexp.MustCompile(`request-(preflight-[0-9a-f]{32})\.json`)

func TestRunRLMRelaySession_PreflightAndRunShareOneLocalSession(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "working-directories.log")
	pythonPath := writeRelaySessionPythonStub(t, `
printf '%s\n' "$PWD" >> "$RLM_SESSION_LOG"
if [ ! -e .preflight-complete ]; then
  touch .preflight-complete
  printf '%s\n' '{"protocol_version":8,"operation":"preflight","status":"success","preflight":{"schema_version":1,"scaffold_manifest":{"id":"`+relaySessionScaffoldID+`","schema_version":2,"inference":{"seed":null}}},"error":null}'
else
  printf '%s\n' '{"protocol_version":8,"operation":"run","answer":"ok","ready":true,"iterations":1,"tokens_used":0,"subcalls":0,"trajectory":[]}'
fi
`)
	t.Setenv("RLM_SESSION_LOG", logPath)

	var (
		mu          sync.Mutex
		requestPath []string
		createCalls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requestPath = append(requestPath, request.URL.Path)
		if got := request.Header.Get("X-ModelRelay-Customer-Id"); got != "customer-123" {
			t.Errorf("%s customer scope = %q, want customer-123", request.URL.Path, got)
		}
		switch request.URL.Path {
		case "/grants/resolve":
			writeRelaySessionJSON(t, w, grantResolutionResponse{
				Profile:                   testRelaySessionProfile(),
				MaxSettledSpendMicrocents: 100,
			})
		case "/grants":
			createCalls++
			workingDirectories := readNonEmptyLines(t, logPath)
			if len(workingDirectories) != 1 {
				t.Errorf("grant created after %d local operations, want exactly one successful preflight", len(workingDirectories))
			}
			writeRelaySessionJSON(t, w, grantCreateResponse{
				GrantID: "execution-1", Credential: "grant-token",
				MaxSettledSpendMicrocents: 60,
			})
		case "/grants/execution-1/finalize":
			writeRelaySessionJSON(t, w, map[string]any{})
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	err := runRLMRelaySession(
		t.Context(), runtimeConfig{BaseURL: server.URL, Output: outputFormatJSON},
		rlmTestProjectAuthority("customer-123"), "test", "question", rlm.ContextPlan{},
		&rlmFlags{pythonPath: pythonPath},
	)
	if err != nil {
		t.Fatalf("runRLMRelaySession error: %v", err)
	}

	workingDirectories := readNonEmptyLines(t, logPath)
	if len(workingDirectories) != 2 {
		t.Fatalf("local operations = %d, want preflight and run", len(workingDirectories))
	}
	if workingDirectories[0] != workingDirectories[1] {
		t.Fatalf("preflight directory %q != run directory %q", workingDirectories[0], workingDirectories[1])
	}
	if _, statErr := os.Stat(workingDirectories[0]); !os.IsNotExist(statErr) {
		t.Fatalf("caller-owned session directory still exists after return: %v", statErr)
	}
	if createCalls != 1 {
		t.Fatalf("grant create calls = %d, want 1", createCalls)
	}
	wantPaths := []string{"/grants/resolve", "/grants", "/grants/execution-1/finalize"}
	if strings.Join(requestPath, "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("request paths = %v, want %v", requestPath, wantPaths)
	}
}

func TestValidateIssuedRLMSpendAuthority(t *testing.T) {
	for _, test := range []struct {
		name     string
		resolved int64
		issued   int64
		wantErr  bool
	}{
		{name: "equal", resolved: 100, issued: 100},
		{name: "remaining allowance tail", resolved: 100, issued: 1},
		{name: "missing", resolved: 100, issued: 0, wantErr: true},
		{name: "negative", resolved: 100, issued: -1, wantErr: true},
		{name: "widened", resolved: 100, issued: 101, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateIssuedRLMSpendAuthority(test.resolved, test.issued)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateIssuedRLMSpendAuthority(%d, %d) = %v, wantErr %v", test.resolved, test.issued, err, test.wantErr)
			}
		})
	}
}

func TestRunRLMRelaySession_PreflightFailureDoesNotCreateLease(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "working-directories.log")
	pythonPath := writeRelaySessionPythonStub(t, `
printf '%s\n' "$PWD" >> "$RLM_SESSION_LOG"
printf '%s\n' 'preflight failed' >&2
exit 1
`)
	t.Setenv("RLM_SESSION_LOG", logPath)

	var (
		mu          sync.Mutex
		requestPath []string
		createCalls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requestPath = append(requestPath, request.URL.Path)
		if request.URL.Path == "/grants/resolve" {
			writeRelaySessionJSON(t, w, grantResolutionResponse{
				Profile: testRelaySessionProfile(), MaxSettledSpendMicrocents: 100,
			})
			return
		}
		if request.URL.Path == "/grants" {
			createCalls++
		}
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	err := runRLMRelaySession(
		t.Context(), runtimeConfig{BaseURL: server.URL, Output: outputFormatJSON},
		rlmTestProjectAuthority("customer-123"), "test", "question", rlm.ContextPlan{},
		&rlmFlags{pythonPath: pythonPath},
	)
	if err == nil || !strings.Contains(err.Error(), "preflight local Droste") {
		t.Fatalf("error = %v, want local preflight failure", err)
	}
	if matched, matchErr := regexp.MatchString(`correlation_id=[0-9a-f]{32}`, err.Error()); matchErr != nil || !matched {
		t.Fatalf("error = %v, want bounded path-safe local correlation ID", err)
	}
	if createCalls != 0 {
		t.Fatalf("grant create calls = %d, want 0", createCalls)
	}
	if len(requestPath) != 1 || requestPath[0] != "/grants/resolve" {
		t.Fatalf("request paths = %v, want resolution only", requestPath)
	}
	workingDirectories := readNonEmptyLines(t, logPath)
	if len(workingDirectories) != 1 {
		t.Fatalf("local operations = %d, want failed preflight only", len(workingDirectories))
	}
	if _, statErr := os.Stat(workingDirectories[0]); !os.IsNotExist(statErr) {
		t.Fatalf("caller-owned session directory still exists after preflight failure: %v", statErr)
	}
}

func TestRunRLMRelaySession_ConcurrentPreflightsUseDistinctRequestFiles(t *testing.T) {
	requestScriptLogs := []string{
		filepath.Join(t.TempDir(), "request-script.log"),
		filepath.Join(t.TempDir(), "request-script.log"),
	}
	pythonPaths := make([]string, len(requestScriptLogs))
	for i, logPath := range requestScriptLogs {
		pythonPaths[i] = writeRelaySessionPythonStub(t, `
printf '%s\n' "$2" >> `+shellSingleQuote(logPath)+`
if [ ! -e .preflight-complete ]; then
  touch .preflight-complete
  printf '%s\n' '{"protocol_version":8,"operation":"preflight","status":"success","preflight":{"schema_version":1,"scaffold_manifest":{"id":"`+relaySessionScaffoldID+`","schema_version":2,"inference":{"seed":null}}},"error":null}'
else
  printf '%s\n' '{"protocol_version":8,"operation":"run","answer":"ok","ready":true,"iterations":1,"tokens_used":0,"subcalls":0,"trajectory":[]}'
fi
`)
	}

	var (
		mu              sync.Mutex
		nextExecutionID int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/grants/resolve":
			writeRelaySessionJSON(t, w, grantResolutionResponse{
				Profile: testRelaySessionProfile(), MaxSettledSpendMicrocents: 100,
			})
		case "/grants":
			mu.Lock()
			nextExecutionID++
			grantID := fmt.Sprintf("execution-%d", nextExecutionID)
			mu.Unlock()
			writeRelaySessionJSON(t, w, grantCreateResponse{
				GrantID: grantID, Credential: "grant-token",
				MaxSettledSpendMicrocents: 100,
			})
		case "/grants/execution-1/finalize", "/grants/execution-2/finalize":
			writeRelaySessionJSON(t, w, map[string]any{})
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	start := make(chan struct{})
	errorsByRun := make(chan error, len(pythonPaths))
	for _, pythonPath := range pythonPaths {
		go func() {
			<-start
			errorsByRun <- runRLMRelaySession(
				t.Context(), runtimeConfig{BaseURL: server.URL, Output: outputFormatJSON},
				rlmTestProjectAuthority("customer-123"), "test", "question", rlm.ContextPlan{},
				&rlmFlags{pythonPath: pythonPath},
			)
		}()
	}
	close(start)
	for range pythonPaths {
		if err := <-errorsByRun; err != nil {
			t.Fatalf("concurrent runRLMRelaySession error: %v", err)
		}
	}

	correlationIDs := make(map[string]struct{}, len(requestScriptLogs))
	for _, logPath := range requestScriptLogs {
		contents, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read request script log: %v", err)
		}
		matches := relayPreflightRequestPathPattern.FindSubmatch(contents)
		if len(matches) != 2 {
			t.Fatalf("request script log missing path-safe preflight request ID: %s", contents)
		}
		correlationIDs[string(matches[1])] = struct{}{}
	}
	if len(correlationIDs) != len(requestScriptLogs) {
		t.Fatalf("concurrent preflight request IDs = %v, want one unique ID per invocation", correlationIDs)
	}
}

func rlmTestProjectAuthority(customerExternalID string) grantAuthority {
	return grantAuthority{apiKey: sdk.SecretKey("mr_sk_test"), customerExternalID: customerExternalID}
}

func testRelaySessionProfile() rlmprofile.ResolvedExecution {
	return rlmprofile.ResolvedExecution{
		Version: rlmprofile.ExecutionProfileVersion, Selector: "preset:test",
		RevisionID:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		RevisionContentHash:  "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		EffectiveFingerprint: "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		Lifecycle:            rlmprofile.LifecycleDefault,
		Root:                 rlmprofile.RoleConfig{Model: pricing.ModelID("root-model"), MaxOutputTokens: 64},
		Subcall:              rlmprofile.RoleConfig{Model: pricing.ModelID("subcall-model"), MaxOutputTokens: 32},
		Limits: rlmprofile.ExecutionLimits{
			MaxSubcalls: 2, MaxDepth: 1, TimeoutMS: 10_000, MaxConcurrency: 1, MaxTotalTokens: 1_000,
		},
	}
}

func writeRelaySessionPythonStub(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "python-stub")
	contents := "#!/bin/sh\nset -eu\n" + strings.TrimSpace(body) + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func writeRelaySessionJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func readNonEmptyLines(t *testing.T, path string) []string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(contents))
}

// These two moved here from the deleted hosted remote-test file (#1895).
// They were always relay-session coverage — one customer authority carried
// across resolve/create/finalize, and the project-key + customer-scope
// requirement — and only lived beside the hosted tests by accident.

func TestDoRLMLeaseJSONUsesOneCustomerAuthority(t *testing.T) {
	t.Parallel()

	var gotPath, gotKey, gotClient, gotCustomer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-ModelRelay-Api-Key")
		gotClient = r.Header.Get("X-ModelRelay-Client")
		gotCustomer = r.Header.Get("X-ModelRelay-Customer-Id")
		var request grantResolutionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Preset != "test" {
			t.Errorf("preset = %q", request.Preset)
		}
		_, _ = w.Write([]byte(`{"profile":{"selector":"preset:test"}}`))
	}))
	t.Cleanup(server.Close)

	var response struct {
		Profile struct {
			Selector string `json:"selector"`
		} `json:"profile"`
	}
	if err := doGrantJSON(t.Context(), server.Client(), server.URL, rlmTestProjectAuthority("customer-123"), http.MethodPost, "/grants/resolve", grantResolutionRequest{Preset: "test"}, &response); err != nil {
		t.Fatalf("doGrantJSON: %v", err)
	}
	if gotPath != "/grants/resolve" || gotKey != "mr_sk_test" || gotClient == "" || gotCustomer != "customer-123" {
		t.Fatalf("request path/key/client/customer = %q/%q/%q/%q", gotPath, gotKey, gotClient, gotCustomer)
	}
	if response.Profile.Selector != "preset:test" {
		t.Fatalf("response selector = %q", response.Profile.Selector)
	}
}

func TestNewRLMLeaseAuthority_RequiresProjectKeyAndCustomerScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   runtimeConfig
		customer string
		wantErr  string
	}{
		{name: "project key with explicit scope", config: runtimeConfig{APIKey: "mr_sk_test"}, customer: " customer-123 "},
		{name: "project key missing scope", config: runtimeConfig{APIKey: "mr_sk_test"}, wantErr: "--customer is required"},
		{name: "account token is not a customer token", config: runtimeConfig{Token: "account-token"}, wantErr: "project API key required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authority, err := newGrantAuthority(tt.config, tt.customer)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("newGrantAuthority: %v", err)
			}
			if authority.apiKey == nil || authority.apiKey.String() != "mr_sk_test" {
				t.Fatalf("api key = %v", authority.apiKey)
			}
			if authority.customerExternalID != "customer-123" {
				t.Fatalf("customer scope = %q", authority.customerExternalID)
			}
		})
	}
}
