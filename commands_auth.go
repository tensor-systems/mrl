package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// loginResponse is the subset of the /auth/login AuthResponse the CLI persists.
type loginResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Account authentication for project/tier admin",
		Long: `Account authentication.

Most mrl commands use a data-plane secret API key (mr_sk_*). Project and tier
administration and API key creation (e.g. 'mrl tier create', 'mrl keys create')
instead require an account bearer token, which 'mrl auth login' obtains and
stores in the active profile.`,
	}
	cmd.AddCommand(newAuthLoginCmd(), newAuthLogoutCmd())
	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	var email string
	var password string
	var passwordStdin bool
	var web bool
	var provider string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in and store an account token (password or --web OAuth)",
		Long: `Obtain a ModelRelay account token and store it in the active profile,
for use by project/tier admin commands.

Two modes:

  # Browser OAuth (GitHub/Google accounts) — opens your browser, no password:
  mrl auth login --web                     # provider defaults to github
  mrl auth login --web --provider google

  # Email + password (password accounts) — via --password-stdin, MODELRELAY_PASSWORD,
  # or --password:
  printf '%s' "$PASS" | mrl auth login --email you@example.com --password-stdin`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := runtimeConfigFrom(cmd)
			if err != nil {
				return err
			}
			if web {
				return runWebLogin(cfg, strings.TrimSpace(provider))
			}
			email = strings.TrimSpace(email)
			if email == "" {
				return errors.New("--email is required (or use --web for browser OAuth)")
			}
			password, err = resolveLoginPassword(password, passwordStdin)
			if err != nil {
				return err
			}

			ctx, cancel := contextWithTimeout(cfg.Timeout)
			defer cancel()

			var resp loginResponse
			if err := doJSON(ctx, cfg, authModeNone, http.MethodPost, "/auth/login",
				map[string]any{"email": email, "password": password}, &resp); err != nil {
				return err
			}
			if strings.TrimSpace(resp.AccessToken) == "" {
				return errors.New("login succeeded but no access token was returned")
			}

			if err := persistAccountToken(cfg.Profile, resp.AccessToken, resp.RefreshToken); err != nil {
				return err
			}
			fmt.Printf("logged in as %s; account token saved to profile %s\n", email, cfg.Profile)
			return nil
		},
	}
	cmd.Flags().BoolVar(&web, "web", false, "Log in via the browser (OAuth) instead of a password")
	cmd.Flags().StringVar(&provider, "provider", "github", "OAuth provider for --web (e.g. github, google)")
	cmd.Flags().StringVar(&email, "email", "", "Account email (password login)")
	cmd.Flags().StringVar(&password, "password", "", "Account password (prefer --password-stdin)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "Read the password from stdin")
	return cmd
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Clear the stored account token for the active profile",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := runtimeConfigFrom(cmd)
			if err != nil {
				return err
			}
			if err := persistAccountToken(cfg.Profile, "", ""); err != nil {
				return err
			}
			fmt.Printf("cleared account token for profile %s\n", cfg.Profile)
			return nil
		},
	}
}

// resolveLoginPassword reads the password from stdin, --password, or the
// MODELRELAY_PASSWORD environment variable, in that order of preference.
func resolveLoginPassword(passwordFlag string, passwordStdin bool) (string, error) {
	if passwordStdin {
		data, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		pass := strings.TrimRight(string(data), "\r\n")
		if pass == "" {
			return "", errors.New("no password received on stdin")
		}
		return pass, nil
	}
	if strings.TrimSpace(passwordFlag) != "" {
		return passwordFlag, nil
	}
	if env := strings.TrimSpace(os.Getenv("MODELRELAY_PASSWORD")); env != "" {
		return env, nil
	}
	return "", errors.New("password required: use --password-stdin, --password, or MODELRELAY_PASSWORD")
}

// persistAccountToken writes the account token (and refresh token) into the named
// profile, leaving all other profile fields untouched.
func persistAccountToken(profileName, token, refreshToken string) error {
	return updateProfile(profileName, func(p *cliProfile) {
		p.Token = token
		p.RefreshToken = refreshToken
	})
}

// errAccountLoginRequired is returned when an account-token command has no
// usable session: no stored token, or one that is expired and cannot be refreshed.
var errAccountLoginRequired = errors.New("not logged in (or the session expired): run 'mrl auth login --web'")

// doAccountJSON performs an account-bearer request. When the server rejects the
// access token with 401 and the profile holds a refresh token, it refreshes the
// session once via /auth/refresh, persists the new tokens to the profile, and
// retries. cfg.Token is updated in place so later calls reuse the new token.
func doAccountJSON(ctx context.Context, cfg *runtimeConfig, method, path string, payload any, out any) error {
	if strings.TrimSpace(cfg.Token) == "" {
		return errAccountLoginRequired
	}
	err := doJSON(ctx, *cfg, authModeBearer, method, path, payload, out)
	if !isUnauthorized(err) {
		return err
	}
	if strings.TrimSpace(cfg.RefreshToken) == "" {
		return errAccountLoginRequired
	}
	var refreshed loginResponse
	if refreshErr := doJSON(ctx, *cfg, authModeNone, http.MethodPost, "/auth/refresh",
		map[string]any{"refresh_token": cfg.RefreshToken}, &refreshed); refreshErr != nil {
		if isUnauthorized(refreshErr) {
			return errAccountLoginRequired
		}
		return fmt.Errorf("refresh account session: %w", refreshErr)
	}
	if strings.TrimSpace(refreshed.AccessToken) == "" {
		return errAccountLoginRequired
	}
	if strings.TrimSpace(refreshed.RefreshToken) == "" {
		refreshed.RefreshToken = cfg.RefreshToken
	}
	if persistErr := persistAccountToken(cfg.Profile, refreshed.AccessToken, refreshed.RefreshToken); persistErr != nil {
		return fmt.Errorf("save refreshed account token: %w", persistErr)
	}
	cfg.Token = refreshed.AccessToken
	cfg.RefreshToken = refreshed.RefreshToken
	err = doJSON(ctx, *cfg, authModeBearer, method, path, payload, out)
	if isUnauthorized(err) {
		return errAccountLoginRequired
	}
	return err
}
