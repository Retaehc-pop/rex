package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Active   ActiveConfig             `toml:"active"`
	Sessions map[string]SessionConfig `toml:"sessions"`
}

type ActiveConfig struct {
	Session string `toml:"session"`
}

type SessionConfig struct {
	Host     string `toml:"host"`
	User     string `toml:"user"`
	Port     int    `toml:"port"`
	Identity string `toml:"identity"`

	// JumpHost, if set, is the bastion/login node to tunnel through.
	// JumpUser defaults to User; JumpPort defaults to 22.
	JumpHost string `toml:"jump_host"`
	JumpUser string `toml:"jump_user"`
	JumpPort int    `toml:"jump_port"`
}

func New() *Config {
	return &Config{Sessions: make(map[string]SessionConfig)}
}

func DefaultPath() string {
	if v := os.Getenv("REX_CONFIG"); v != "" {
		return v
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "rex", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "rex", "config.toml")
}
