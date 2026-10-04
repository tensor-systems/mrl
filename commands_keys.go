package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	sdk "github.com/modelrelay/modelrelay/sdk/go"
	"github.com/spf13/cobra"
)

type apiKeyResponse struct {
	APIKey sdk.APIKey `json:"api_key"`
}

type apiKeyListResponse struct {
	APIKeys []sdk.APIKey `json:"api_keys"`
}

// accountMeResponse is the subset of GET /auth/me the CLI needs: the account's
// default project, created at signup.
type accountMeResponse struct {
	User struct {
		ProjectID uuid.UUID `json:"project_id"`
	} `json:"user"`
}

type accountProject struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type accountProjectListResponse struct {
	Projects []accountProject `json:"projects"`
}

func newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage project API keys (requires 'mrl auth login')",
		Long: `Create and list secret API keys (mr_sk_*).

API key administration uses the account token from 'mrl auth login' (for
example 'mrl auth login --web'), not an API key.`,
	}
	cmd.AddCommand(newKeysCreateCmd(), newKeysListCmd())
	return cmd
}

func newKeysCreateCmd() *cobra.Command {
	var name string
	var printOnly bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a secret API key and save it to the active profile",
		Long: `Create a secret API key (mr_sk_*) in a project.

The project is --project / MODELRELAY_PROJECT_ID / the profile's project_id,
otherwise the account's default project.

By default the key is printed once and saved as the active profile's API key
(as 'mrl config set --api-key' does). With --print, only the secret is written
to stdout, newline-terminated, and nothing is saved:

  mrl auth login --web
  mrl keys create --name laptop
  KEY=$(mrl keys create --name hark --print)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := runtimeConfigFrom(cmd)
			if err != nil {
				return err
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return errors.New("--name is required")
			}
			if strings.TrimSpace(cfg.Token) == "" {
				return errAccountLoginRequired
			}

			ctx, cancel := contextWithTimeout(cfg.Timeout)
			defer cancel()

			projectID, err := resolveKeyProject(ctx, &cfg)
			if err != nil {
				return err
			}

			var resp apiKeyResponse
			payload := map[string]any{
				"label":      name,
				"project_id": projectID.String(),
				"kind":       string(sdk.APIKeyKindSecret),
			}
			if err = doAccountJSON(ctx, &cfg, http.MethodPost, "/api-keys", payload, &resp); err != nil {
				return err
			}
			key := resp.APIKey
			if strings.TrimSpace(key.SecretKey) == "" {
				return errors.New("API key created but no secret was returned")
			}

			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if printOnly {
				_, _ = fmt.Fprintf(stderr, "created API key %s (%s) in project %s\n", key.ID, key.Label, key.ProjectID)
				_, err = fmt.Fprintln(stdout, key.SecretKey)
				return err
			}

			if err = updateProfile(cfg.Profile, func(p *cliProfile) { p.setAPIKey(key.SecretKey) }); err != nil {
				return fmt.Errorf("API key %s was created but saving it to profile %s failed: %w", key.ID, cfg.Profile, err)
			}
			if cfg.Output == outputFormatJSON {
				printJSON(key)
				return nil
			}
			fprintKeyValueTable(stdout, []kvPair{
				{Key: "id", Value: key.ID.String()},
				{Key: "label", Value: key.Label},
				{Key: "redacted_key", Value: key.RedactedKey},
				{Key: "project_id", Value: key.ProjectID.String()},
			})
			_, _ = fmt.Fprintf(stdout, "\nsecret (shown once): %s\n", key.SecretKey)
			_, _ = fmt.Fprintf(stdout, "saved as the API key for profile %s\n", cfg.Profile)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Label for the key (required)")
	cmd.Flags().BoolVar(&printOnly, "print", false, "Print only the secret to stdout; do not save it to the profile")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newKeysListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the account's API keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := runtimeConfigFrom(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := contextWithTimeout(cfg.Timeout)
			defer cancel()

			var resp apiKeyListResponse
			if err := doAccountJSON(ctx, &cfg, http.MethodGet, "/api-keys", nil, &resp); err != nil {
				return err
			}
			if cfg.Output == outputFormatJSON {
				printJSON(resp)
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tLABEL\tKEY\tPROJECT\tCREATED")
			for i := range resp.APIKeys {
				k := &resp.APIKeys[i]
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					k.ID, k.Label, k.RedactedKey, k.ProjectID, k.CreatedAt.Format("2006-01-02"))
			}
			return w.Flush()
		},
	}
}

// resolveKeyProject picks the project a new key belongs to: the explicit
// project (--project / MODELRELAY_PROJECT_ID / profile), else the account's
// default project from /auth/me, else the account's only project.
func resolveKeyProject(ctx context.Context, cfg *runtimeConfig) (uuid.UUID, error) {
	if explicit := strings.TrimSpace(cfg.ProjectID); explicit != "" {
		id, err := uuid.Parse(explicit)
		if err != nil {
			return uuid.Nil, fmt.Errorf("invalid project id %q: must be a UUID", explicit)
		}
		return id, nil
	}

	var me accountMeResponse
	if err := doAccountJSON(ctx, cfg, http.MethodGet, "/auth/me", nil, &me); err != nil {
		return uuid.Nil, err
	}
	if me.User.ProjectID != uuid.Nil {
		return me.User.ProjectID, nil
	}

	var list accountProjectListResponse
	if err := doAccountJSON(ctx, cfg, http.MethodGet, "/projects", nil, &list); err != nil {
		return uuid.Nil, err
	}
	switch len(list.Projects) {
	case 0:
		return uuid.Nil, errors.New("account has no projects: create one in the dashboard, then pass --project")
	case 1:
		return list.Projects[0].ID, nil
	}
	var b strings.Builder
	b.WriteString("account has several projects and no default; pass --project with one of:")
	for _, p := range list.Projects {
		fmt.Fprintf(&b, "\n  %s  %s", p.ID, p.Name)
	}
	return uuid.Nil, errors.New(b.String())
}
