package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ModelPrice holds pricing for a model.
type ModelPrice struct {
	InputPerMtok  float64 `json:"input_per_mtok"`
	OutputPerMtok float64 `json:"output_per_mtok"`
}

// Config holds the proxy configuration.
type Config struct {
	Port              int                   `json:"port"`
	SessionGapMinutes int64                 `json:"session_gap_minutes"`
	StatuslinePath    string                `json:"statusline_path"`
	Inspect           bool                  `json:"inspect"`
	Pricing           map[string]ModelPrice `json:"pricing"`
	DefaultModel      string                `json:"default_model"`
	Mode              string                `json:"mode"`
	ContextWindows    map[string]int64      `json:"context_windows"`
}

// Default returns the built-in defaults.
func Default() *Config {
	return &Config{
		Port:              7474,
		SessionGapMinutes: 30,
		StatuslinePath:    "~/.files/states/ctx.json",
		Inspect:           false,
		Pricing: map[string]ModelPrice{
			"claude-sonnet-4": {InputPerMtok: 3.00, OutputPerMtok: 15.00},
			"claude-haiku-4":  {InputPerMtok: 0.80, OutputPerMtok: 4.00},
			"claude-opus-4":   {InputPerMtok: 15.00, OutputPerMtok: 75.00},
		},
		DefaultModel: "claude-sonnet-4",
		Mode:         "context",
		ContextWindows: map[string]int64{
			"claude-sonnet-4": 200000,
			"claude-haiku-4":  200000,
			"claude-opus-4":   200000,
		},
	}
}

// ContextWindow returns the context window size for the given model.
// Falls back to 200000 if the model is not found.
func (c *Config) ContextWindow(model string) int64 {
	if c.ContextWindows != nil {
		if w, ok := c.ContextWindows[model]; ok {
			return w
		}
	}
	return 200000
}

// Dir returns the config directory path.
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "claude-context-proxy")
}

// Path returns the config file path.
func Path() string {
	return filepath.Join(Dir(), "config.json")
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// Load loads the config file, applying env overrides.
// Returns a merged config: defaults < file < env vars.
func Load() *Config {
	cfg := Default()

	data, err := os.ReadFile(Path())
	if err == nil {
		var fileCfg Config
		if json.Unmarshal(data, &fileCfg) == nil {
			if fileCfg.Port != 0 {
				cfg.Port = fileCfg.Port
			}
			if fileCfg.SessionGapMinutes != 0 {
				cfg.SessionGapMinutes = fileCfg.SessionGapMinutes
			}
			if fileCfg.StatuslinePath != "" {
				cfg.StatuslinePath = fileCfg.StatuslinePath
			}
			if fileCfg.Inspect {
				cfg.Inspect = fileCfg.Inspect
			}
			if len(fileCfg.Pricing) > 0 {
				cfg.Pricing = fileCfg.Pricing
			}
			if fileCfg.DefaultModel != "" {
				cfg.DefaultModel = fileCfg.DefaultModel
			}
			if fileCfg.Mode != "" {
				cfg.Mode = fileCfg.Mode
			}
			if len(fileCfg.ContextWindows) > 0 {
				cfg.ContextWindows = fileCfg.ContextWindows
			}
		}
	}

	if v := os.Getenv("CTX_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	}
	if v := os.Getenv("CTX_SESSION_GAP_MINUTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.SessionGapMinutes = n
		}
	}
	// CTX_STATUSLINE_PATH can be set to empty string to disable.
	if v, ok := os.LookupEnv("CTX_STATUSLINE_PATH"); ok {
		cfg.StatuslinePath = v
	}
	if os.Getenv("CTX_INSPECT") == "1" {
		cfg.Inspect = true
	}
	if v := os.Getenv("CTX_MODE"); v == "context" || v == "cost" {
		cfg.Mode = v
	}

	return cfg
}

// EnsureFile creates the config file with defaults if it doesn't exist.
func EnsureFile() {
	path := Path()
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		log.Printf("config: mkdir: %v", err)
		return
	}
	cfg := Default()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("config: write defaults: %v", err)
	}
}
