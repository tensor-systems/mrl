package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Device sign-in (OAuth 2.0 Device Authorization Grant, RFC 8628): for a
// machine whose browser the user can't reach — a headless server, or a remote
// Mac driven by an agent while the user chats from a phone. mrl prints a link
// with the code filled in; the user opens it anywhere, signs in (or signs up)
// on the dashboard, and taps Approve; mrl polls until then.

const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// deviceCodeResponse is POST /auth/device/code's body.
type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceTokenError is the token endpoint's RFC 6749 §5.2 error body.
type deviceTokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval"`
}

// deviceLogin runs one device sign-in. sleep is injectable for tests.
type deviceLogin struct {
	cfg    runtimeConfig
	stdout io.Writer
	stderr io.Writer
	sleep  func(time.Duration)
	now    func() time.Time
}

func runDeviceLogin(cfg runtimeConfig, stdout, stderr io.Writer) error {
	return deviceLogin{cfg: cfg, stdout: stdout, stderr: stderr, sleep: time.Sleep, now: time.Now}.run()
}

func (d deviceLogin) run() error {
	ctx, cancel := contextWithTimeout(d.cfg.Timeout)
	var code deviceCodeResponse
	err := doJSON(ctx, d.cfg, authModeNone, http.MethodPost, "/auth/device/code", map[string]any{"client_id": "mrl"}, &code)
	cancel()
	if err != nil {
		return fmt.Errorf("start device sign-in: %w", err)
	}
	if code.DeviceCode == "" || code.UserCode == "" || code.VerificationURI == "" {
		return errors.New("start device sign-in: incomplete response from server")
	}
	link := code.VerificationURIComplete
	if link == "" {
		link = code.VerificationURI
	}
	interval := time.Duration(max(code.Interval, 5)) * time.Second
	expiresIn := time.Duration(max(code.ExpiresIn, 60)) * time.Second

	// The link and code go to stdout first, on their own, so an agent can relay
	// them while mrl keeps polling.
	if d.cfg.Output == outputFormatJSON {
		if err = writeJSONLine(d.stdout, map[string]any{
			"status":                    "pending",
			"verification_uri_complete": link,
			"verification_uri":          code.VerificationURI,
			"user_code":                 code.UserCode,
			"expires_in":                int(expiresIn.Seconds()),
			"interval":                  int(interval.Seconds()),
		}); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(d.stdout, "Open %s and approve code %s\n", link, code.UserCode)
		_, _ = fmt.Fprintf(d.stderr, "Waiting for approval (the code expires in %d minutes)...\n", int(expiresIn.Round(time.Minute).Minutes()))
	}

	tokens, err := d.poll(code.DeviceCode, interval, d.now().Add(expiresIn))
	if err != nil {
		return err
	}
	if err = persistAccountToken(d.cfg.Profile, tokens.AccessToken, tokens.RefreshToken); err != nil {
		return err
	}
	if d.cfg.Output == outputFormatJSON {
		return writeJSONLine(d.stdout, map[string]any{"status": "logged_in", "profile": d.cfg.Profile})
	}
	_, _ = fmt.Fprintf(d.stdout, "logged in via device sign-in; account token saved to profile %s\n", d.cfg.Profile)
	return nil
}

// poll asks for tokens every interval until approval, denial or expiry.
func (d deviceLogin) poll(deviceCode string, interval time.Duration, deadline time.Time) (loginResponse, error) {
	for {
		d.sleep(interval)
		if !d.now().Before(deadline) {
			return loginResponse{}, errDeviceCodeExpired
		}
		ctx, cancel := contextWithTimeout(d.cfg.Timeout)
		var tokens loginResponse
		err := doJSON(ctx, d.cfg, authModeNone, http.MethodPost, "/auth/device/token",
			map[string]any{"grant_type": deviceGrantType, "device_code": deviceCode}, &tokens)
		cancel()
		if err == nil {
			if strings.TrimSpace(tokens.AccessToken) == "" {
				return loginResponse{}, errors.New("device sign-in approved but no access token was returned")
			}
			return tokens, nil
		}

		var statusErr *httpStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode >= http.StatusInternalServerError {
			// Network trouble or a server hiccup: keep trying until the code expires.
			_, _ = fmt.Fprintf(d.stderr, "device sign-in: retrying after error: %v\n", err)
			continue
		}
		var tokenErr deviceTokenError
		if statusErr.StatusCode != http.StatusBadRequest || json.Unmarshal([]byte(statusErr.Body), &tokenErr) != nil {
			return loginResponse{}, fmt.Errorf("device sign-in: %w", err)
		}
		switch tokenErr.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			// RFC 8628 §3.5: add 5 seconds, or adopt the server's interval if larger.
			interval += 5 * time.Second
			if server := time.Duration(tokenErr.Interval) * time.Second; server > interval {
				interval = server
			}
			continue
		case "access_denied":
			return loginResponse{}, errors.New("device sign-in was denied")
		case "expired_token":
			return loginResponse{}, errDeviceCodeExpired
		default:
			return loginResponse{}, fmt.Errorf("device sign-in failed: %s %s", tokenErr.Error, tokenErr.ErrorDescription)
		}
	}
}

var errDeviceCodeExpired = errors.New("device sign-in code expired before it was approved; run 'mrl auth login --device' again")

func writeJSONLine(w io.Writer, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
