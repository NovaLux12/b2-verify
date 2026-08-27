package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	kindB2   = "b2"
	kindFile = "file"

	defaultListen          = ":9118"
	defaultIntervalSeconds = 300
	defaultConfigPath      = "config.json"
)

// Config is the top-level configuration. The file may be either a bare JSON
// array of legs or an object with a "legs" array plus optional settings.
type Config struct {
	Legs            []Leg  `json:"legs"`
	Listen          string `json:"listen,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
}

// Leg describes one monitored backup destination.
type Leg struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind"` // "b2" or "file"
	MaxAgeHours float64 `json:"max_age_hours"`

	// kind == "b2"
	Bucket   string `json:"bucket,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	KeyIDEnv string `json:"key_id_env,omitempty"`
	KeyEnv   string `json:"key_env,omitempty"`

	// kind == "file"
	Path string `json:"path,omitempty"`
}

// loadConfig reads and validates the configuration file from disk.
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// parseConfig decodes a configuration document and applies defaults.
func parseConfig(data []byte) (Config, error) {
	var cfg Config
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var legs []Leg
		if err := json.Unmarshal(trimmed, &legs); err != nil {
			return cfg, fmt.Errorf("parse legs array: %w", err)
		}
		cfg.Legs = legs
	} else {
		if err := json.Unmarshal(trimmed, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}
	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = defaultIntervalSeconds
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if len(c.Legs) == 0 {
		return errors.New("config must define at least one leg")
	}
	seen := map[string]bool{}
	for i, leg := range c.Legs {
		if err := leg.validate(); err != nil {
			return fmt.Errorf("leg %d (%s): %w", i, leg.Name, err)
		}
		if seen[leg.Name] {
			return fmt.Errorf("duplicate leg name %q", leg.Name)
		}
		seen[leg.Name] = true
	}
	return nil
}

func (l Leg) validate() error {
	if strings.TrimSpace(l.Name) == "" {
		return errors.New("name must not be empty")
	}
	if l.MaxAgeHours <= 0 {
		return fmt.Errorf("max_age_hours must be positive, got %v", l.MaxAgeHours)
	}
	switch l.Kind {
	case kindB2:
		if l.Bucket == "" {
			return errors.New(`kind "b2" requires bucket`)
		}
		if l.KeyIDEnv == "" {
			return errors.New(`kind "b2" requires key_id_env`)
		}
		if l.KeyEnv == "" {
			return errors.New(`kind "b2" requires key_env`)
		}
	case kindFile:
		if l.Path == "" {
			return errors.New(`kind "file" requires path`)
		}
	default:
		return fmt.Errorf("kind must be %q or %q, got %q", kindB2, kindFile, l.Kind)
	}
	return nil
}
