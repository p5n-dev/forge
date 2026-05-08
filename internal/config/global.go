package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type GlobalConfig struct {
	Forgejo ForgejoConfig `yaml:"forgejo"`
	Image   ImageConfig   `yaml:"image"`
	SSH     SSHConfig     `yaml:"ssh"`
}

type ForgejoConfig struct {
	URL        string `yaml:"url"`
	Token      string `yaml:"token"`
	Port       int    `yaml:"port,omitempty"`
	AdminUser  string `yaml:"admin_user,omitempty"`
	AdminToken string `yaml:"admin_token,omitempty"`
}

type ImageConfig struct {
	CacheDir string `yaml:"cache_dir"`
}

type SSHConfig struct {
	InjectUserKey bool   `yaml:"inject_user_key"`
	UserKeyPath   string `yaml:"user_key_path"`
}

func DefaultGlobal() GlobalConfig {
	return GlobalConfig{
		Image: ImageConfig{
			CacheDir: "~/.forge/images",
		},
		SSH: SSHConfig{
			InjectUserKey: false,
			UserKeyPath:   "~/.ssh/id_ed25519.pub",
		},
	}
}

func LoadGlobal(path string) (GlobalConfig, error) {
	cfg := DefaultGlobal()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return GlobalConfig{}, fmt.Errorf("reading %s: %w", path, err)
	}

	if len(data) == 0 {
		return cfg, nil
	}

	// Unmarshal into defaults so unset fields keep their default values.
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return GlobalConfig{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	applyGlobalDefaults(&cfg)

	return cfg, nil
}

func applyGlobalDefaults(cfg *GlobalConfig) {
	if cfg.Image.CacheDir == "" {
		cfg.Image.CacheDir = "~/.forge/images"
	}
	if cfg.SSH.UserKeyPath == "" {
		cfg.SSH.UserKeyPath = "~/.ssh/id_ed25519.pub"
	}
}

// SaveGlobal persists the GlobalConfig to the given path. The parent directory
// is created if it does not already exist.
func SaveGlobal(path string, cfg GlobalConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating config dir %s: %w", dir, err)
		}
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
