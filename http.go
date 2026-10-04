package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type authMode int

const (
	authModeNone authMode = iota
	authModeAPIKey
	// authModeBearer authenticates with an account bearer token (from
	// 'mrl auth login'), required for project/tier admin routes.
	authModeBearer
)

func doJSON(ctx context.Context, cfg runtimeConfig, mode authMode, method, path string, payload any, out any) error {
	body, err := doJSONRaw(ctx, cfg, mode, method, path, payload)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return err
	}
	return nil
}

func doJSONRaw(ctx context.Context, cfg runtimeConfig, mode authMode, method, path string, payload any) ([]byte, error) {
	fullURL, err := joinBaseURL(cfg.BaseURL, path)
	if err != nil {
		return nil, err
	}

	var body io.Reader
	if payload != nil {
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return nil, marshalErr
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authErr := applyAuth(req, cfg, mode); authErr != nil {
		return nil, authErr
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return nil, &httpStatusError{StatusCode: resp.StatusCode, Body: msg}
	}

	return data, nil
}

// httpStatusError is returned by doJSONRaw for any non-2xx response, so callers
// can branch on the status (e.g. refresh an expired account token on 401).
type httpStatusError struct {
	StatusCode int
	Body       string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("request failed: status=%d body=%s", e.StatusCode, e.Body)
}

func isUnauthorized(err error) bool {
	var statusErr *httpStatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusUnauthorized
}

func applyAuth(req *http.Request, cfg runtimeConfig, mode authMode) error {
	switch mode {
	case authModeNone:
		return nil
	case authModeAPIKey:
		if strings.TrimSpace(cfg.APIKey) == "" {
			return errors.New("api key required")
		}
		req.Header.Set("X-ModelRelay-Api-Key", strings.TrimSpace(cfg.APIKey))
	case authModeBearer:
		if strings.TrimSpace(cfg.Token) == "" {
			return errors.New("account token required: run 'mrl auth login'")
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.Token))
	default:
		return errors.New("invalid auth mode")
	}
	return nil
}

func joinBaseURL(baseURL, path string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("invalid base url: %w", err)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if u.Path == "" {
		u.Path = "/"
	}
	// Separate path and query string to avoid encoding the query string
	pathPart, queryPart, _ := strings.Cut(path, "?")
	u.Path = strings.TrimRight(u.Path, "/") + pathPart
	if queryPart != "" {
		u.RawQuery = queryPart
	}
	return u.String(), nil
}
