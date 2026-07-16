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
  printf '%s\n' '{"protocol_version":6,"operation":"preflight","status":"success","preflight":{"schema_version":1,"scaffold_manifest":{"id":"`+relaySessionScaffoldID+`","schema_version":1,"inference":{"seed":null}}},"error":null}'
else
  printf '%s\n' '{"protocol_version":6,"operation":"run","answer":"ok","ready":true,"iterations":1,"tokens_used":0,"subcalls":0,"trajectory":[]}'
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
		switch request.URL.Path {
		case "/rlm/executions/resolve":
			writeRelaySessionJSON(t, w, rlmLeaseResolutionResponse{
				Profile:              testRelaySessionProfile(),
				MaxSettledSpendCents: 100,
			})
		case "/rlm/executions":
			createCalls++
			workingDirectories := readNonEmptyLines(t, logPath)
			if len(workingDirectories) != 1 {
				t.Errorf("lease created after %d local operations, want exactly one successful preflight", len(workingDirectories))
			}
			writeRelaySessionJSON(t, w, rlmLeaseCreateResponse{
				ExecutionID: "execution-1", Credential: "lease-token",
				RootCallbackPath: "/rlm/root", SubcallCallbackPath: "/rlm/subcall",
				MaxSettledSpendCents: 100,
			})
		case "/rlm/executions/execution-1/finalize":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	err := runRLMRelaySession(
		t.Context(), runtimeConfig{BaseURL: server.URL, Output: outputFormatJSON},
		sdk.SecretKey("mr_sk_test"), "preset:test", "question", rlm.ContextPlan{},
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
		t.Fatalf("lease create calls = %d, want 1", createCalls)
	}
	wantPaths := []string{"/rlm/executions/resolve", "/rlm/executions", "/rlm/executions/execution-1/finalize"}
	if strings.Join(requestPath, "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("request paths = %v, want %v", requestPath, wantPaths)
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
		if request.URL.Path == "/rlm/executions/resolve" {
			writeRelaySessionJSON(t, w, rlmLeaseResolutionResponse{
				Profile: testRelaySessionProfile(), MaxSettledSpendCents: 100,
			})
			return
		}
		if request.URL.Path == "/rlm/executions" {
			createCalls++
		}
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	err := runRLMRelaySession(
		t.Context(), runtimeConfig{BaseURL: server.URL, Output: outputFormatJSON},
		sdk.SecretKey("mr_sk_test"), "preset:test", "question", rlm.ContextPlan{},
		&rlmFlags{pythonPath: pythonPath},
	)
	if err == nil || !strings.Contains(err.Error(), "preflight local Droste") {
		t.Fatalf("error = %v, want local preflight failure", err)
	}
	if matched, matchErr := regexp.MatchString(`correlation_id=[0-9a-f]{32}`, err.Error()); matchErr != nil || !matched {
		t.Fatalf("error = %v, want bounded path-safe local correlation ID", err)
	}
	if createCalls != 0 {
		t.Fatalf("lease create calls = %d, want 0", createCalls)
	}
	if len(requestPath) != 1 || requestPath[0] != "/rlm/executions/resolve" {
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
  printf '%s\n' '{"protocol_version":6,"operation":"preflight","status":"success","preflight":{"schema_version":1,"scaffold_manifest":{"id":"`+relaySessionScaffoldID+`","schema_version":1,"inference":{"seed":null}}},"error":null}'
else
  printf '%s\n' '{"protocol_version":6,"operation":"run","answer":"ok","ready":true,"iterations":1,"tokens_used":0,"subcalls":0,"trajectory":[]}'
fi
`)
	}

	var (
		mu              sync.Mutex
		nextExecutionID int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/rlm/executions/resolve":
			writeRelaySessionJSON(t, w, rlmLeaseResolutionResponse{
				Profile: testRelaySessionProfile(), MaxSettledSpendCents: 100,
			})
		case "/rlm/executions":
			mu.Lock()
			nextExecutionID++
			executionID := fmt.Sprintf("execution-%d", nextExecutionID)
			mu.Unlock()
			writeRelaySessionJSON(t, w, rlmLeaseCreateResponse{
				ExecutionID: executionID, Credential: "lease-token",
				RootCallbackPath: "/rlm/root", SubcallCallbackPath: "/rlm/subcall",
				MaxSettledSpendCents: 100,
			})
		case "/rlm/executions/execution-1/finalize", "/rlm/executions/execution-2/finalize":
			w.WriteHeader(http.StatusNoContent)
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
				sdk.SecretKey("mr_sk_test"), "preset:test", "question", rlm.ContextPlan{},
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
