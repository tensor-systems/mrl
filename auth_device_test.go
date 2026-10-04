package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// deviceTestServer scripts the token endpoint: each poll pops the next reply.
type deviceTestServer struct {
	t       *testing.T
	mu      sync.Mutex
	replies []func(w http.ResponseWriter)
	polls   int
	bodies  []map[string]string
}

func (s *deviceTestServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/auth/device/code":
		_, _ = w.Write([]byte(`{"device_code":"dev-secret","user_code":"WDJB-MJHT",` +
			`"verification_uri":"https://modelrelay.ai/device",` +
			`"verification_uri_complete":"https://modelrelay.ai/device?code=WDJB-MJHT",` +
			`"expires_in":600,"interval":5}`))
	case "/auth/device/token":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.bodies = append(s.bodies, body)
		if s.polls >= len(s.replies) {
			s.t.Errorf("unexpected poll %d", s.polls+1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reply := s.replies[s.polls]
		s.polls++
		reply(w)
	default:
		s.t.Errorf("unexpected path %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func tokenError(code string, interval int) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": code, "interval": interval})
	}
}

func tokenSuccess(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"access_token":"acct-access","refresh_token":"acct-refresh",` +
		`"tokens":{"access_token":"acct-access","refresh_token":"acct-refresh"},"user":{"email":"u@example.com"}}`))
}

// fakeDeviceClock advances only when the device login sleeps.
type fakeDeviceClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeDeviceClock) sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
}

func runTestDeviceLogin(t *testing.T, srv *deviceTestServer, output outputFormat) (string, string, *fakeDeviceClock, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	t.Cleanup(server.Close)
	clock := &fakeDeviceClock{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	var stdout, stderr bytes.Buffer
	err := deviceLogin{
		cfg:    runtimeConfig{Profile: "default", BaseURL: server.URL, Timeout: time.Second, Output: output},
		stdout: &stdout,
		stderr: &stderr,
		sleep:  clock.sleep,
		now:    func() time.Time { return clock.now },
	}.run()
	return stdout.String(), stderr.String(), clock, err
}

func TestDeviceLogin_PendingThenApprovedSavesTokens(t *testing.T) {
	useTempConfig(t)
	srv := &deviceTestServer{t: t, replies: []func(http.ResponseWriter){
		tokenError("authorization_pending", 0),
		tokenError("authorization_pending", 0),
		tokenSuccess,
	}}
	stdout, stderr, clock, err := runTestDeviceLogin(t, srv, outputFormatTable)
	if err != nil {
		t.Fatalf("device login: %v (stderr %s)", err, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if lines[0] != "Open https://modelrelay.ai/device?code=WDJB-MJHT and approve code WDJB-MJHT" {
		t.Fatalf("first stdout line = %q", lines[0])
	}
	if !strings.Contains(stdout, "account token saved to profile default") {
		t.Fatalf("stdout = %q", stdout)
	}
	if srv.polls != 3 {
		t.Fatalf("polls = %d, want 3", srv.polls)
	}
	for _, d := range clock.sleeps {
		if d != 5*time.Second {
			t.Fatalf("polled at %v, want the server's 5s interval", clock.sleeps)
		}
	}
	if srv.bodies[0]["device_code"] != "dev-secret" || srv.bodies[0]["grant_type"] != deviceGrantType {
		t.Fatalf("token request body = %v", srv.bodies[0])
	}

	// Saved exactly as --web saves them, so account commands work next.
	cfg, err := loadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	profile := profileFor(cfg, "default")
	if profile.Token != "acct-access" || profile.RefreshToken != "acct-refresh" {
		t.Fatalf("profile tokens = %q / %q", profile.Token, profile.RefreshToken)
	}
}

func TestDeviceLogin_SlowDownBacksOff(t *testing.T) {
	useTempConfig(t)
	srv := &deviceTestServer{t: t, replies: []func(http.ResponseWriter){
		tokenError("slow_down", 10),
		tokenError("slow_down", 0),
		tokenSuccess,
	}}
	_, _, clock, err := runTestDeviceLogin(t, srv, outputFormatTable)
	if err != nil {
		t.Fatalf("device login: %v", err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second}
	if len(clock.sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", clock.sleeps, want)
	}
	for i := range want {
		if clock.sleeps[i] != want[i] {
			t.Fatalf("sleeps = %v, want %v", clock.sleeps, want)
		}
	}
}

func TestDeviceLogin_ExpiredAndDenied(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{"expired_token", "expired"},
		{"access_denied", "denied"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			useTempConfig(t)
			srv := &deviceTestServer{t: t, replies: []func(http.ResponseWriter){tokenError(tc.code, 0)}}
			_, _, _, err := runTestDeviceLogin(t, srv, outputFormatTable)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			cfg, _ := loadCLIConfig()
			if profileFor(cfg, "default").Token != "" {
				t.Fatal("a token was saved after a failed sign-in")
			}
		})
	}
}

func TestDeviceLogin_StopsAtLocalDeadline(t *testing.T) {
	useTempConfig(t)
	// The server never answers anything but pending; mrl gives up once the
	// code's lifetime (600s at 5s polls = 119 polls) has passed.
	replies := make([]func(http.ResponseWriter), 200)
	for i := range replies {
		replies[i] = tokenError("authorization_pending", 0)
	}
	srv := &deviceTestServer{t: t, replies: replies}
	_, _, _, err := runTestDeviceLogin(t, srv, outputFormatTable)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want expiry", err)
	}
	if srv.polls > 120 {
		t.Fatalf("polled %d times past the code's lifetime", srv.polls)
	}
}

func TestDeviceLogin_JSONEmitsLinkFirst(t *testing.T) {
	useTempConfig(t)
	srv := &deviceTestServer{t: t, replies: []func(http.ResponseWriter){tokenSuccess}}
	stdout, _, _, err := runTestDeviceLogin(t, srv, outputFormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout lines = %q", lines)
	}
	var first, last map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["verification_uri_complete"] != "https://modelrelay.ai/device?code=WDJB-MJHT" || first["user_code"] != "WDJB-MJHT" || first["status"] != "pending" {
		t.Fatalf("first line = %v", first)
	}
	if _, leaked := first["device_code"]; leaked {
		t.Fatal("device_code must not be printed")
	}
	if err := json.Unmarshal([]byte(lines[1]), &last); err != nil {
		t.Fatal(err)
	}
	if last["status"] != "logged_in" || last["profile"] != "default" {
		t.Fatalf("last line = %v", last)
	}
}

func TestDeviceLogin_RetriesServerErrors(t *testing.T) {
	useTempConfig(t)
	srv := &deviceTestServer{t: t, replies: []func(http.ResponseWriter){
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
		tokenSuccess,
	}}
	_, stderr, _, err := runTestDeviceLogin(t, srv, outputFormatTable)
	if err != nil {
		t.Fatalf("device login: %v", err)
	}
	if !strings.Contains(stderr, "retrying") {
		t.Fatalf("stderr = %q", stderr)
	}
}
