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

// SessionConfig is an ordered list of nodes forming the connection chain.
// The first node is dialled directly; each subsequent node is reached by
// tunnelling through the previous one. The last node is the target.
type SessionConfig struct {
	Name  string       `toml:"name,omitempty"`
	Nodes []NodeConfig `toml:"nodes"`
}

type NodeConfig struct {
	Name     string `toml:"name,omitempty"`     // optional human label
	Host     string `toml:"host"`
	User     string `toml:"user"`
	Port     int    `toml:"port,omitempty"`     // default 22
	Identity string `toml:"identity,omitempty"` // path to private key; falls back to ssh-agent
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
