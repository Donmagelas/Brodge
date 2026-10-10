// Package config parses only the local Desktop/IM bridge configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	DataDir    string           `toml:"data_dir"`
	Language   string           `toml:"language"`
	Management ManagementConfig `toml:"management"`
	Projects   []ProjectConfig  `toml:"projects"`
}
type ManagementConfig struct {
	Enabled bool   `toml:"enabled"`
	Port    int    `toml:"port"`
	Token   string `toml:"token"`
}
type ProjectConfig struct {
	Name      string           `toml:"name"`
	Agent     AgentConfig      `toml:"agent"`
	Platforms []PlatformConfig `toml:"platforms"`
}
type AgentConfig struct {
	Type    string         `toml:"type"`
	Options map[string]any `toml:"options"`
}
type PlatformConfig struct {
	Type    string         `toml:"type"`
	Options map[string]any `toml:"options"`
}

func Load(path string) (*Config, error) {
	var cfg Config
	metadata, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("cannot parse bridge config")
	}
	if len(metadata.Undecoded()) > 0 {
		return nil, fmt.Errorf("unsupported config field: %s", metadata.Undecoded()[0])
	}
	cfg.Management.Token = os.ExpandEnv(cfg.Management.Token)
	for i := range cfg.Projects {
		resolveOptions(cfg.Projects[i].Agent.Options)
		for j := range cfg.Projects[i].Platforms {
			resolveOptions(cfg.Projects[i].Platforms[j].Options)
		}
	}
	if !cfg.Management.Enabled || strings.TrimSpace(cfg.Management.Token) == "" {
		return nil, fmt.Errorf("local management must be enabled with a nonempty token")
	}
	if cfg.Management.Port == 0 {
		cfg.Management.Port = 18789
	}
	if cfg.Management.Port < 1 || cfg.Management.Port > 65535 {
		return nil, fmt.Errorf("invalid management port")
	}
	if len(cfg.Projects) != 1 {
		return nil, fmt.Errorf("this version requires one Desktop connection")
	}
	p := cfg.Projects[0]
	if p.Name == "" || filepath.Base(p.Name) != p.Name || p.Name == "." || p.Name == ".." {
		return nil, fmt.Errorf("invalid connection name")
	}
	if p.Agent.Type != "codex-desktop" {
		return nil, fmt.Errorf("only codex-desktop is supported")
	}
	if len(p.Platforms) > 1 {
		return nil, fmt.Errorf("only one Feishu connection is supported")
	}
	for _, platform := range p.Platforms {
		if platform.Type != "feishu" {
			return nil, fmt.Errorf("only Feishu is supported in this version")
		}
	}
	if cfg.DataDir == "" {
		cfg.DataDir = ".state"
	}
	cfg.DataDir = expandHome(os.ExpandEnv(cfg.DataDir))
	cfg.DataDir, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}
func resolveOptions(options map[string]any) {
	for k, v := range options {
		switch v := v.(type) {
		case string:
			options[k] = os.ExpandEnv(v)
		case map[string]any:
			resolveOptions(v)
		}
	}
}
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
