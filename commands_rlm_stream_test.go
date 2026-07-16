package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelrelay/modelrelay/sdk/go"
)

func rlmStreamTestFrame(runID, parentRunID, eventType string, sequence, depth int) string {
	parent := ""
	if parentRunID != "" {
		parent = fmt.Sprintf(`, "parent_run_id": %q`, parentRunID)
	}
	return fmt.Sprintf(
		`{ "run_id": %q%s, "type": %q, "seq": %d, "depth": %d, "version": 2 }`,
		runID, parent, eventType, sequence, depth,
	)
}

func validRLMTestStream() string {
	return strings.Join([]string{
		rlmStreamTestFrame("root", "", "startup", 1, 0),
		rlmStreamTestFrame("root", "", "iteration_start", 2, 0),
		rlmStreamTestFrame("root", "", "result", 3, 0),
		rlmStreamTestFrame("root", "", "done", 4, 0),
	}, "\n") + "\n"
}

func TestNewRLMCmd_StreamFlag_IsRegistered(t *testing.T) {
	flag := newRLMCmd().Flags().Lookup("stream")
	if flag == nil {
		t.Fatal("stream flag is not registered")
	}
	if flag.DefValue != "false" {
		t.Fatalf("stream default = %q, want false", flag.DefValue)
	}
}

func TestValidateRLMStreamMode_IncompatibleModes_AreRejected(t *testing.T) {
	tests := []struct {
		name  string
		cfg   runtimeConfig
		flags rlmFlags
		want  string
	}{
		{name: "local", flags: rlmFlags{stream: true}, want: "requires --remote"},
		{name: "relay session", flags: rlmFlags{stream: true, remote: true, relaySession: true}, want: "--relay-session"},
		{name: "json flag or profile", cfg: runtimeConfig{Output: outputFormatJSON}, flags: rlmFlags{stream: true, remote: true}, want: "--json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRLMStreamMode(test.cfg, &test.flags)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateRLMStreamMode() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateRLMStreamMode_RemoteTableOutput_IsAccepted(t *testing.T) {
	err := validateRLMStreamMode(runtimeConfig{Output: outputFormatTable}, &rlmFlags{stream: true, remote: true})
	if err != nil {
		t.Fatalf("validateRLMStreamMode() error = %v", err)
	}
}

func TestExecuteRLMRemoteStream_ValidResponse_PreservesExactBytes(t *testing.T) {
	wantStream := validRLMTestStream()
	var gotAccept, gotKey, gotClient string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		gotAccept = request.Header.Get("Accept")
		gotKey = request.Header.Get("X-ModelRelay-Api-Key")
		gotClient = request.Header.Get("X-ModelRelay-Client")
		if request.URL.Path != "/rlm/execute" {
			t.Errorf("path = %q, want /rlm/execute", request.URL.Path)
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request Content-Type = %q", request.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if !bytes.Contains(body, []byte(`"model":"demo"`)) {
			t.Errorf("request body = %s, want model", body)
		}
		w.Header().Set("Content-Type", rlmStreamMediaType)
		_, _ = io.WriteString(w, wantStream)
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	err := executeRLMRemoteStream(
		context.Background(), server.Client(), server.URL, sdk.SecretKey("mr_sk_test"),
		rlmExecuteRemoteRequest{Model: "demo", Query: "hi"}, &stdout,
	)
	if err != nil {
		t.Fatalf("executeRLMRemoteStream() error = %v", err)
	}
	if gotAccept != rlmStreamMediaType {
		t.Fatalf("Accept = %q, want %q", gotAccept, rlmStreamMediaType)
	}
	if gotKey != "mr_sk_test" || gotClient == "" {
		t.Fatalf("key/client = %q/%q", gotKey, gotClient)
	}
	if stdout.String() != wantStream {
		t.Fatalf("stdout changed stream bytes\ngot:  %q\nwant: %q", stdout.String(), wantStream)
	}
}

func TestExecuteRLMRemoteStream_ContentTypeVariants(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		wantErr     bool
	}{
		{name: "quoted profile", contentType: rlmStreamMediaType},
		{name: "invalid unquoted profile", contentType: "application/x-ndjson; profile=rlm-stream/v1", wantErr: true},
		{name: "missing profile", contentType: "application/x-ndjson", wantErr: true},
		{name: "wrong profile", contentType: `application/x-ndjson; profile="other"`, wantErr: true},
		{name: "extra parameter", contentType: rlmStreamMediaType + "; charset=utf-8", wantErr: true},
		{name: "wrong media", contentType: `application/json; profile="rlm-stream/v1"`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, validRLMTestStream())
			}))
			t.Cleanup(server.Close)

			var stdout bytes.Buffer
			err := executeRLMRemoteStream(t.Context(), server.Client(), server.URL, nil, rlmExecuteRemoteRequest{}, &stdout)
			if test.wantErr && err == nil {
				t.Fatal("executeRLMRemoteStream() error = nil, want Content-Type error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("executeRLMRemoteStream() error = %v", err)
			}
			if test.wantErr && stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no bytes before Content-Type validation", stdout.String())
			}
		})
	}
}

func TestExecuteRLMRemoteStream_NonSuccess_DoesNotWriteStdout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"denied"}`, http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	err := executeRLMRemoteStream(t.Context(), server.Client(), server.URL, nil, rlmExecuteRemoteRequest{}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("executeRLMRemoteStream() error = %v, want status and response detail", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestCopyAndValidateRLMStream_NestedRunAndReplay_PreservesExactBytes(t *testing.T) {
	stream := strings.Join([]string{
		rlmStreamTestFrame("root", "", "startup", 1, 0),
		rlmStreamTestFrame("child", "root", "startup", 1, 1),
		rlmStreamTestFrame("child", "root", "result", 2, 1),
		rlmStreamTestFrame("child", "root", "done", 3, 1),
		rlmStreamTestFrame("root", "", "result", 2, 0),
		rlmStreamTestFrame("root", "", "replay", 3, 0),
		rlmStreamTestFrame("root", "", "done", 4, 0),
	}, "\n") + "\n"

	var stdout bytes.Buffer
	if err := copyAndValidateRLMStream(&stdout, strings.NewReader(stream)); err != nil {
		t.Fatalf("copyAndValidateRLMStream() error = %v", err)
	}
	if stdout.String() != stream {
		t.Fatalf("stdout changed stream bytes\ngot:  %q\nwant: %q", stdout.String(), stream)
	}
}

func TestCopyAndValidateRLMStream_ChildStartupMayArriveFirst(t *testing.T) {
	stream := strings.Join([]string{
		rlmStreamTestFrame("child", "root", "startup", 1, 1),
		rlmStreamTestFrame("root", "", "progress", 1, 0),
		rlmStreamTestFrame("root", "", "result", 2, 0),
		rlmStreamTestFrame("root", "", "done", 3, 0),
	}, "\n") + "\n"
	var stdout bytes.Buffer
	if err := copyAndValidateRLMStream(&stdout, strings.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != stream {
		t.Fatal("stdout changed child-first stream bytes")
	}
}

func TestCopyAndValidateRLMStream_CanonicalOutcomeVariants(t *testing.T) {
	for _, eventType := range []string{"execution_error", "extract"} {
		t.Run(eventType, func(t *testing.T) {
			stream := strings.Join([]string{
				rlmStreamTestFrame("root", "", "startup", 1, 0),
				rlmStreamTestFrame("root", "", eventType, 2, 0),
				rlmStreamTestFrame("root", "", "result", 3, 0),
				rlmStreamTestFrame("root", "", "done", 4, 0),
			}, "\n") + "\n"
			var stdout bytes.Buffer
			if err := copyAndValidateRLMStream(&stdout, strings.NewReader(stream)); err != nil {
				t.Fatal(err)
			}
			if stdout.String() != stream {
				t.Fatalf("stdout changed %s stream bytes", eventType)
			}
		})
	}
}

func TestCopyAndValidateRLMStream_IncompleteOrInvalid_ReturnsErrorAfterExactPartialOutput(t *testing.T) {
	startup := rlmStreamTestFrame("root", "", "startup", 1, 0) + "\n"
	result := rlmStreamTestFrame("root", "", "result", 2, 0) + "\n"
	replayBeforeResult := rlmStreamTestFrame("root", "", "replay", 2, 0) + "\n"
	progressAfterResult := rlmStreamTestFrame("root", "", "progress", 3, 0) + "\n"
	done := rlmStreamTestFrame("root", "", "done", 2, 0) + "\n"
	incompleteHierarchy := startup +
		rlmStreamTestFrame("child", "missing-parent", "startup", 1, 1) + "\n" +
		result + rlmStreamTestFrame("root", "", "done", 3, 0) + "\n"
	tests := []struct {
		name   string
		stream string
		want   string
	}{
		{name: "empty", stream: "", want: "empty"},
		{name: "truncated", stream: strings.TrimSuffix(startup, "\n"), want: "truncated"},
		{name: "missing result", stream: startup + done, want: "preceded its result"},
		{name: "replay before result", stream: startup + replayBeforeResult, want: "preceded its result"},
		{name: "unexpected event after result", stream: startup + result + progressAfterResult, want: "terminal suffix"},
		{name: "missing done", stream: startup + result, want: "final root done"},
		{name: "malformed", stream: startup + "not-json\n", want: "malformed"},
		{name: "incomplete hierarchy", stream: incompleteHierarchy, want: "incomplete run hierarchy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := copyAndValidateRLMStream(&stdout, strings.NewReader(test.stream))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("copyAndValidateRLMStream() error = %v, want containing %q", err, test.want)
			}
			if stdout.String() != test.stream {
				t.Fatalf("stdout = %q, want exact partial stream %q", stdout.String(), test.stream)
			}
		})
	}
}

func TestCopyAndValidateRLMStream_RootDoneMustBeFinalFrame(t *testing.T) {
	stream := validRLMTestStream() + rlmStreamTestFrame("child", "root", "startup", 1, 1) + "\n"
	var stdout bytes.Buffer
	err := copyAndValidateRLMStream(&stdout, strings.NewReader(stream))
	if err == nil || !strings.Contains(err.Error(), "follows root result") {
		t.Fatalf("copyAndValidateRLMStream() error = %v, want terminal ordering error", err)
	}
	if stdout.String() != stream {
		t.Fatalf("stdout changed stream bytes: %q", stdout.String())
	}
}

type rlmStreamFailingWriter struct{}

func (rlmStreamFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("closed pipe")
}

func TestCopyAndValidateRLMStream_StdoutFailure_IsReturned(t *testing.T) {
	err := copyAndValidateRLMStream(rlmStreamFailingWriter{}, strings.NewReader(validRLMTestStream()))
	if err == nil || !strings.Contains(err.Error(), "closed pipe") {
		t.Fatalf("copyAndValidateRLMStream() error = %v, want stdout failure", err)
	}
}
