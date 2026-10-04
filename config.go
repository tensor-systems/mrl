package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type cliConfig struct {
	CurrentProfile string                `toml:"current_profile,omitempty"`
	Profiles       map[string]cliProfile `toml:"profiles,omitempty"`
}

type cliProfile struct {
	APIKey       string   `toml:"api_key,omitempty"`
	Token        string   `toml:"token,omitempty"`
	RefreshToken string   `toml:"refresh_token,omitempty"`
	BaseURL      string   `toml:"base_url,omitempty"`
	ProjectID    string   `toml:"project_id,omitempty"`
	Output       string   `toml:"output,omitempty"`
	Model        string   `toml:"model,omitempty"`
	AllowAll     bool     `toml:"allow_all,omitempty"`
	Allow        []string `toml:"allow,omitempty"`
	Trace        bool     `toml:"trace,omitempty"`
}

func loadCLIConfig() (cliConfig, error) {
	path, err := defaultConfigPath()
	if err != nil {
		return cliConfig{}, err
	}
	return readCLIConfig(path)
}

func defaultConfigPath() (string, error) {
	// Respect XDG_CONFIG_HOME if set, otherwise use ~/.config
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "mrl", "config.toml"), nil
}

func readCLIConfig(path string) (cliConfig, error) {
	if strings.TrimSpace(path) == "" {
		return cliConfig{}, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is the user's standard or explicitly configured mrl config
	if err != nil {
		if os.IsNotExist(err) {
			return cliConfig{}, nil
		}
		return cliConfig{}, err
	}
	if len(data) == 0 {
		return cliConfig{}, nil
	}
	var cfg cliConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return cliConfig{}, err
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]cliProfile{}
	}
	return cfg, nil
}

func writeCLIConfig(cfg cliConfig) error {
	path, err := defaultConfigPath()
	if err != nil {
		return err
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]cliProfile{}
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
		return mkdirErr
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // path is the user's mrl config destination
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func resolveProfileName(flagValue string, cfg cliConfig) string {
	clean := strings.TrimSpace(flagValue)
	if clean != "" {
		return clean
	}
	clean = strings.TrimSpace(cfg.CurrentProfile)
	if clean != "" {
		return clean
	}
	return "default"
}

func profileFor(cfg cliConfig, name string) cliProfile {
	if cfg.Profiles == nil {
		return cliProfile{}
	}
	return cfg.Profiles[name]
}

// updateProfile loads the config file, applies mutate to the named profile, and
// writes it back, leaving every other profile field and profile untouched. It
// also makes the profile current when no current profile is set yet.
func updateProfile(profileName string, mutate func(*cliProfile)) error {
	cfg, err := loadCLIConfig()
	if err != nil {
		return err
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]cliProfile{}
	}
	profileCfg := profileFor(cfg, profileName)
	mutate(&profileCfg)
	cfg.Profiles[profileName] = profileCfg
	if cfg.CurrentProfile == "" {
		cfg.CurrentProfile = profileName
	}
	return writeCLIConfig(cfg)
}

// setAPIKey sets the profile's data-plane API key. 'mrl config set --api-key'
// and 'mrl keys create' both store keys through it.
func (p *cliProfile) setAPIKey(apiKey string) {
	p.APIKey = strings.TrimSpace(apiKey)
}
