package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type outputFormat string

const (
	outputFormatJSON  outputFormat = "json"
	outputFormatTable outputFormat = "table"
)

type runtimeConfig struct {
	Profile   string
	BaseURL   string
	ProjectID string
	APIKey    string
	Token     string
	// RefreshToken is the profile's account refresh token. It is only set when
	// Token itself came from the profile, so a --token/MODELRELAY_TOKEN override
	// is never silently replaced by a refreshed profile session.
	RefreshToken string
	Model        string
	Output       outputFormat
	Timeout      time.Duration
	AllowAll     bool
	Allow        []string
	Trace        bool
}

type runtimeConfigKey struct{}

func withRuntimeConfig(ctx context.Context, cfg runtimeConfig) context.Context {
	return context.WithValue(ctx, runtimeConfigKey{}, cfg)
}

func runtimeConfigFrom(cmd *cobra.Command) (runtimeConfig, error) {
	val := cmd.Context().Value(runtimeConfigKey{})
	if val == nil {
		return runtimeConfig{}, errors.New("runtime config unavailable")
	}
	cfg, ok := val.(runtimeConfig)
	if !ok {
		return runtimeConfig{}, errors.New("runtime config invalid")
	}
	return cfg, nil
}

func resolveRuntimeConfig(cmd *cobra.Command, cfgFile cliConfig) (runtimeConfig, error) {
	profileFlag, _ := cmd.Flags().GetString("profile")
	profileName := resolveProfileName(profileFlag, cfgFile)
	profile := profileFor(cfgFile, profileName)

	baseFlag, _ := cmd.Flags().GetString("base-url")
	projectFlag, _ := cmd.Flags().GetString("project")
	apiKeyFlag, _ := cmd.Flags().GetString("api-key")
	tokenFlag, _ := cmd.Flags().GetString("token")
	jsonFlag, _ := cmd.Flags().GetBool("json")
	timeoutFlag, _ := cmd.Flags().GetDuration("timeout")

	baseURL := firstNonEmpty(baseFlag, os.Getenv("MODELRELAY_API_BASE_URL"), profile.BaseURL, defaultAPIBaseURL)
	if strings.TrimSpace(baseURL) == "" {
		return runtimeConfig{}, errors.New("base URL is required")
	}

	projectID := firstNonEmpty(projectFlag, os.Getenv("MODELRELAY_PROJECT_ID"), profile.ProjectID)
	apiKey := firstNonEmpty(apiKeyFlag, os.Getenv("MODELRELAY_API_KEY"), os.Getenv("MODELRELAY_SECRET_KEY"), profile.APIKey)
	token := firstNonEmpty(tokenFlag, os.Getenv("MODELRELAY_TOKEN"), profile.Token)
	model := firstNonEmpty(os.Getenv("MODELRELAY_MODEL"), profile.Model)
	refreshToken := ""
	if strings.TrimSpace(firstNonEmpty(tokenFlag, os.Getenv("MODELRELAY_TOKEN"))) == "" {
		refreshToken = profile.RefreshToken
	}

	output, err := resolveOutputFormat(jsonFlag, profile.Output)
	if err != nil {
		return runtimeConfig{}, err
	}

	timeout := 30 * time.Second
	if timeoutFlag > 0 {
		timeout = timeoutFlag
	}

	return runtimeConfig{
		Profile:      profileName,
		BaseURL:      strings.TrimSpace(baseURL),
		ProjectID:    strings.TrimSpace(projectID),
		APIKey:       strings.TrimSpace(apiKey),
		Token:        strings.TrimSpace(token),
		RefreshToken: strings.TrimSpace(refreshToken),
		Model:        strings.TrimSpace(model),
		Output:       output,
		Timeout:      timeout,
		AllowAll:     profile.AllowAll,
		Allow:        profile.Allow,
		Trace:        profile.Trace,
	}, nil
}

func resolveOutputFormat(jsonFlag bool, profileOutput string) (outputFormat, error) {
	if jsonFlag {
		return outputFormatJSON, nil
	}
	raw := strings.TrimSpace(profileOutput)
	if raw == "" {
		return outputFormatTable, nil
	}
	switch strings.ToLower(raw) {
	case string(outputFormatJSON):
		return outputFormatJSON, nil
	case string(outputFormatTable):
		return outputFormatTable, nil
	default:
		return "", fmt.Errorf("invalid output format: %s", raw)
	}
}

func contextWithTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), timeout)
}

func firstNonEmpty(values ...string) string {
	for _, val := range values {
		if strings.TrimSpace(val) != "" {
			return val
		}
	}
	return ""
}
