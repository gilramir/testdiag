// Package config loads testdiag configuration from a TOML file under
// ~/.config/testdiag/config.toml, with environment-variable overrides.
//
// Environment variables always win over the file, so secrets can be injected
// at runtime (e.g. in CI) without writing them to disk.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Config is the fully-resolved configuration for a testdiag run.
type Config struct {
	Jenkins   Jenkins   `toml:"jenkins"`
	LLM       LLM       `toml:"llm"`
	Workspace Workspace `toml:"workspace"`
	Output    Output    `toml:"output"`
}

// Jenkins holds credentials for talking to the Jenkins HTTP API. Jenkins uses
// HTTP Basic auth with the user's login and a personal API token (NOT the
// account password): Authorization: Basic base64(user:apiToken).
type Jenkins struct {
	User     string `toml:"user"`
	APIToken string `toml:"api_token"`
}

// LLM points at any OpenAI-API-compatible endpoint (including a local server).
type LLM struct {
	Provider    string  `toml:"provider"`    // "openai" for OpenAI-compatible servers
	BaseURL     string  `toml:"base_url"`    // e.g. http://localhost:1234/v1
	Model       string  `toml:"model"`       // model name the server exposes
	APIKey      string  `toml:"api_key"`     // may be empty for local servers
	Temperature float32 `toml:"temperature"` // sampling temperature
	MaxTokens   int     `toml:"max_tokens"`  // max tokens per completion

	// NormalizeToolCalls runs requests through an in-process proxy that rewrites
	// each model's native tool-call syntax (GPT-OSS/Gemma/Mistral/Nemotron) into
	// the form AgenticGoKit parses. On by default; harmless if the model already
	// uses a recognized format. Disable for a model that needs no translation.
	NormalizeToolCalls bool `toml:"normalize_tool_calls"`
	// InjectTools makes that proxy add a `tools` array to each request so
	// tool-aware chat templates advertise the tools to the model. On by default;
	// disable if a server rejects requests that carry tools.
	InjectTools bool `toml:"inject_tools"`
	// Debug logs the full request/response conversation with the LLM to stderr.
	// Off by default; the --debug CLI flag also turns it on.
	Debug bool `toml:"debug"`
}

// Workspace describes the local checkout the failing tests came from. The
// file-reading tools are jailed to Root.
type Workspace struct {
	// Root is the absolute path to the git workspace the Jenkins build ran
	// against. If empty, the current working directory is used. TEST_AGENT.md
	// is expected at the root of this directory.
	Root string `toml:"root"`
}

// Output controls how diagnosis reports are written.
type Output struct {
	Dir     string `toml:"dir"`     // directory for the markdown reports
	Workers int    `toml:"workers"` // parallel worker count (per-test independence)
}

// Path returns the default config file location.
func Path() (string, error) {
	dir, err := os.UserConfigDir() // ~/.config on Linux
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "testdiag", "config.toml"), nil
}

// Load reads the config file (if present) and applies environment overrides.
// A missing file is not an error: env vars alone may provide everything.
func Load() (*Config, error) {
	cfg := defaults()

	path, err := Path()
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("reading %s: %w", path, statErr)
	}

	applyEnvOverrides(cfg)

	if cfg.Workspace.Root == "" {
		if wd, err := os.Getwd(); err == nil {
			cfg.Workspace.Root = wd
		}
	}
	if abs, err := filepath.Abs(cfg.Workspace.Root); err == nil {
		cfg.Workspace.Root = abs
	}

	return cfg, cfg.validate()
}

func defaults() *Config {
	return &Config{
		LLM: LLM{
			Provider:           "openai",
			Temperature:        0.0,
			MaxTokens:          4096,
			NormalizeToolCalls: true,
			InjectTools:        true,
		},
		Output: Output{
			Dir:     "test-diagnosis",
			Workers: 4,
		},
	}
}

// applyEnvOverrides lets every field be set/overridden from the environment.
func applyEnvOverrides(cfg *Config) {
	setStr(&cfg.Jenkins.User, "TESTDIAG_JENKINS_USER")
	setStr(&cfg.Jenkins.APIToken, "TESTDIAG_JENKINS_TOKEN")

	setStr(&cfg.LLM.Provider, "TESTDIAG_LLM_PROVIDER")
	setStr(&cfg.LLM.BaseURL, "TESTDIAG_LLM_BASE_URL")
	setStr(&cfg.LLM.Model, "TESTDIAG_LLM_MODEL")
	setStr(&cfg.LLM.APIKey, "TESTDIAG_LLM_API_KEY")
	setFloat(&cfg.LLM.Temperature, "TESTDIAG_LLM_TEMPERATURE")
	setInt(&cfg.LLM.MaxTokens, "TESTDIAG_LLM_MAX_TOKENS")
	setBool(&cfg.LLM.NormalizeToolCalls, "TESTDIAG_LLM_NORMALIZE_TOOL_CALLS")
	setBool(&cfg.LLM.InjectTools, "TESTDIAG_LLM_INJECT_TOOLS")
	setBool(&cfg.LLM.Debug, "TESTDIAG_LLM_DEBUG")

	setStr(&cfg.Workspace.Root, "TESTDIAG_WORKSPACE_ROOT")

	setStr(&cfg.Output.Dir, "TESTDIAG_OUTPUT_DIR")
	setInt(&cfg.Output.Workers, "TESTDIAG_WORKERS")
}

func (c *Config) validate() error {
	if c.LLM.Model == "" {
		return fmt.Errorf("llm.model is required (set in config or TESTDIAG_LLM_MODEL)")
	}
	if c.LLM.BaseURL == "" {
		return fmt.Errorf("llm.base_url is required for an OpenAI-compatible endpoint")
	}
	if c.Output.Workers < 1 {
		c.Output.Workers = 1
	}
	return nil
}

func setStr(dst *string, env string) {
	if v, ok := os.LookupEnv(env); ok {
		*dst = v
	}
}

func setInt(dst *int, env string) {
	if v, ok := os.LookupEnv(env); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func setBool(dst *bool, env string) {
	if v, ok := os.LookupEnv(env); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

func setFloat(dst *float32, env string) {
	if v, ok := os.LookupEnv(env); ok {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			*dst = float32(f)
		}
	}
}
