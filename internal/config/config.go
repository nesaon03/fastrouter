// Package config loads the fastrouter config file.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Duration is a time.Duration that reads and writes as "30s" in JSON.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

// Config is the on-disk configuration (fastrouter.json in the data dir).
type Config struct {
	// Host defaults to 127.0.0.1. Set "0.0.0.0" to expose on the network (opt-in).
	Host string `json:"host"`
	Port int    `json:"port"`
	// DataDir holds fastrouter.db and secret.key. Defaults to the config file's dir.
	DataDir string `json:"dataDir"`

	ConnectTimeout   Duration `json:"connectTimeout"`
	FirstByteTimeout Duration `json:"firstByteTimeout"` // upstream must start answering within this
	IdleTimeout      Duration `json:"idleTimeout"`      // max gap between streamed chunks
	CooldownBase     Duration `json:"cooldownBase"`     // account cooldown after a failure
	MaxIdleConnsPer  int      `json:"maxIdleConnsPerHost"`

	// EmptyResponseWait is how long a stream is held back waiting for its first
	// real content, so an empty answer can fail over to the next account
	// (9Router behaviour). After this the stream is passed on regardless.
	EmptyResponseWait Duration `json:"emptyResponseWait"`

	// KeyLimits are optional per-key limits, keyed by local API key name or id.
	// Empty (the default) means no limits beyond 9Router's own access lists.
	KeyLimits map[string]KeyLimit `json:"keyLimits,omitempty"`
}

// KeyLimit limits one local API key.
type KeyLimit struct {
	RPM    int      `json:"rpm"`    // requests per minute, 0 = unlimited
	Models []string `json:"models"` // allowed models/combos ("*" globs allowed); empty = all
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Host:             "127.0.0.1",
		Port:             20128,
		ConnectTimeout:   Duration(10 * time.Second),
		FirstByteTimeout: Duration(120 * time.Second),
		IdleTimeout:      Duration(90 * time.Second),
		CooldownBase:     Duration(30 * time.Second),
		MaxIdleConnsPer:  64,

		EmptyResponseWait: Duration(45 * time.Second),
	}
}

// DefaultPath is ~/.fastrouter/fastrouter.json.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".fastrouter", "fastrouter.json")
}

// Load reads path, writing a default file there if it does not exist yet.
func Load(path string) (Config, error) {
	cfg := Default()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return cfg, err
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		out, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
			return cfg, err
		}
	case err != nil:
		return cfg, err
	default:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, err
		}
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Dir(path)
	}
	return cfg, os.MkdirAll(cfg.DataDir, 0o700)
}
