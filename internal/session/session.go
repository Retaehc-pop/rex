package session

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"rex/config"
)

func Load(path string) (*config.Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return config.New(), nil
	}
	var cfg config.Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Sessions == nil {
		cfg.Sessions = make(map[string]config.SessionConfig)
	}
	return &cfg, nil
}

func Save(path string, cfg *config.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

// ParseTarget parses "user@host[:port]" into components.
func ParseTarget(target string) (user, host string, port int, err error) {
	port = 22
	at := strings.LastIndex(target, "@")
	if at < 0 {
		return "", "", 0, fmt.Errorf("invalid target %q: expected user@host[:port]", target)
	}
	user = target[:at]
	hostport := target[at+1:]

	if strings.Contains(hostport, ":") {
		h, p, e := net.SplitHostPort(hostport)
		if e != nil {
			return "", "", 0, fmt.Errorf("invalid host:port %q: %w", hostport, e)
		}
		host = h
		port, err = strconv.Atoi(p)
		if err != nil {
			return "", "", 0, fmt.Errorf("invalid port %q", p)
		}
	} else {
		host = hostport
	}
	return
}

// Set registers a session. jump is optional ("" means no jump host).
// jump format: "user@host[:port]"
func Set(cfg *config.Config, name, target, jump string) error {
	user, host, port, err := ParseTarget(target)
	if err != nil {
		return err
	}
	sess := config.SessionConfig{
		Host: host,
		User: user,
		Port: port,
		Jump: jump, // stored verbatim; parsed at connect time
	}
	if jump != "" {
		// Validate jump target syntax up front.
		if _, _, _, err := ParseTarget(jump); err != nil {
			return fmt.Errorf("jump host: %w", err)
		}
	}
	cfg.Sessions[name] = sess
	cfg.Active.Session = name
	return nil
}

func Use(cfg *config.Config, name string) error {
	if _, ok := cfg.Sessions[name]; !ok {
		return fmt.Errorf("session %q not found", name)
	}
	cfg.Active.Session = name
	return nil
}

func Active(cfg *config.Config) (config.SessionConfig, error) {
	name := cfg.Active.Session
	if name == "" {
		return config.SessionConfig{}, fmt.Errorf("no active session. Run: rex --set-session user@host")
	}
	s, ok := cfg.Sessions[name]
	if !ok {
		return config.SessionConfig{}, fmt.Errorf("active session %q not found in config", name)
	}
	return s, nil
}

func Get(cfg *config.Config, name string) (config.SessionConfig, error) {
	s, ok := cfg.Sessions[name]
	if !ok {
		return config.SessionConfig{}, fmt.Errorf("session %q not found", name)
	}
	return s, nil
}
