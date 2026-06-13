// Package config loads testdiag configuration from a TOML file under
// ~/.config/testdiag/config.toml, with environment-variable overrides.
//
// Environment variables always win over the file, so secrets can be injected
// at runtime (e.g. in CI) without writing them to disk.
//
// The workflow is a state machine of stages (DOWNLOAD → LOGPARSE → DEEPINSPECT).
// LLMs are defined once under [llms.<name>] and each stage points at one by
// name under [stages]; this lets a cheap model summarize the log while a
// stronger model does the deep source tracing.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Stage names. They double as the keys in the [stages] table.
const (
	StageLogParse         = "logparse"
	StageLogParseFeedback = "logparse_feedback" // optional; falls back to logparse LLM
	StageDeepInspect      = "deepinspect"
)

// Config is the fully-resolved configuration for a testdiag run.
type Config struct {
	Jenkins   Jenkins            `toml:"jenkins"`
	LLMs      map[string]LLMSpec `toml:"llms"`   // named LLMs, referenced by stages
	Stages    map[string]string  `toml:"stages"` // stage name -> LLM name
	Proxy     Proxy              `toml:"proxy"`
	Workspace Workspace          `toml:"workspace"`
	Output    Output             `toml:"output"`
	Diagnosis Diagnosis          `toml:"diagnosis"`
}

// Jenkins holds credentials for talking to the Jenkins HTTP API. Jenkins uses
// HTTP Basic auth with the user's login and a personal API token (NOT the
// account password): Authorization: Basic base64(user:apiToken).
type Jenkins struct {
	User     string `toml:"user"`
	APIToken string `toml:"api_token"`
}

// LLMSpec is one named LLM endpoint. Several may be defined and assigned to
// different stages. Each points at any OpenAI-API-compatible server (including a
// local one).
type LLMSpec struct {
	Provider      string  `toml:"provider"`       // "openai" for OpenAI-compatible servers
	BaseURL       string  `toml:"base_url"`       // e.g. http://localhost:1234/v1
	Model         string  `toml:"model"`          // model name the server exposes
	APIKey        string  `toml:"api_key"`        // may be empty for local servers
	ContextWindow int     `toml:"context_window"` // model context size in tokens (0 = unknown)
	Temperature   float32 `toml:"temperature"`    // sampling temperature
	MaxTokens     int     `toml:"max_tokens"`     // max tokens per completion
}

// Proxy configures the in-process normalizing reverse proxy that fronts each LLM
// endpoint. These knobs are global because they apply identically to every
// endpoint the stages talk to.
type Proxy struct {
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

// Diagnosis tunes the per-test agent loops.
type Diagnosis struct {
	// MaxAttempts is the total number of agent attempts per test, including the
	// first. When >1, a critique/revise feedback loop re-runs the agent with the
	// previous draft and the specific gaps fed back, whenever an attempt looks
	// shallow (didn't explore source / didn't identify a flakiness mechanism).
	// A value of 1 disables the loop.
	MaxAttempts int `toml:"max_attempts"`
	// MaxToolIterations caps the native tool-calling loop within a SINGLE attempt
	// (one agent.Run): how many times the agent may call a tool and feed the
	// result back before it must produce an answer. The worst-case number of LLM
	// round-trips per test is therefore MaxAttempts * MaxToolIterations.
	MaxToolIterations int `toml:"max_tool_iterations"`
	// MaxLogParseFeedbacks is the number of times the FEEDBACK stage may reject
	// a LOGPARSE brief before the test is abandoned. 0 disables feedback entirely.
	MaxLogParseFeedbacks int `toml:"max_logparse_feedbacks"`
}

// Output controls how diagnosis reports are written.
type Output struct {
	Dir string `toml:"dir"` // directory for the markdown reports
}

// LLMForStage resolves the LLM assigned to a stage. It errors clearly if the
// stage has no assignment or names an LLM that is not defined.
func (c *Config) LLMForStage(stage string) (LLMSpec, error) {
	name, ok := c.Stages[stage]
	if !ok || name == "" {
		return LLMSpec{}, fmt.Errorf("no LLM assigned to stage %q (set [stages].%s = \"<llm-name>\")", stage, stage)
	}
	spec, ok := c.LLMs[name]
	if !ok {
		return LLMSpec{}, fmt.Errorf("stage %q references undefined LLM %q (define [llms.%s])", stage, name, name)
	}
	return spec, nil
}

// LLMForStageOptional resolves the LLM for a stage that may have no explicit
// assignment. Unlike LLMForStage it does not error on a missing or unknown
// assignment — it returns (zero, false) instead, letting the caller supply a
// fallback. Use this for optional stages like logparse_feedback.
func (c *Config) LLMForStageOptional(stage string) (LLMSpec, bool) {
	name, ok := c.Stages[stage]
	if !ok || name == "" {
		return LLMSpec{}, false
	}
	spec, ok := c.LLMs[name]
	return spec, ok
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

	cfg.normalizeLLMs()
	return cfg, cfg.validate()
}

func defaults() *Config {
	return &Config{
		Proxy: Proxy{
			NormalizeToolCalls: true,
			InjectTools:        true,
		},
		Output: Output{
			Dir: "test-diagnosis",
		},
		Diagnosis: Diagnosis{
			MaxAttempts:          3,
			MaxToolIterations:    50,
			MaxLogParseFeedbacks: 2,
		},
	}
}

// defaultMaxTokens is applied to any named LLM that doesn't set max_tokens.
const defaultMaxTokens = 4096

// normalizeLLMs fills per-LLM defaults that TOML's zero values can't express
// (an unset provider/max_tokens shouldn't stay empty/zero).
func (c *Config) normalizeLLMs() {
	for name, spec := range c.LLMs {
		if spec.Provider == "" {
			spec.Provider = "openai"
		}
		if spec.MaxTokens <= 0 {
			spec.MaxTokens = defaultMaxTokens
		}
		c.LLMs[name] = spec
	}
}

// applyEnvOverrides lets every field be set/overridden from the environment.
// Named-LLM secrets follow the pattern TESTDIAG_LLM_<NAME>_{API_KEY,BASE_URL,
// MODEL}, where <NAME> is the LLM's name upper-cased with non-alphanumerics
// turned into underscores (e.g. [llms.fast] -> TESTDIAG_LLM_FAST_API_KEY).
func applyEnvOverrides(cfg *Config) {
	setStr(&cfg.Jenkins.User, "TESTDIAG_JENKINS_USER")
	setStr(&cfg.Jenkins.APIToken, "TESTDIAG_JENKINS_TOKEN")

	for name, spec := range cfg.LLMs {
		prefix := "TESTDIAG_LLM_" + envName(name) + "_"
		setStr(&spec.APIKey, prefix+"API_KEY")
		setStr(&spec.BaseURL, prefix+"BASE_URL")
		setStr(&spec.Model, prefix+"MODEL")
		cfg.LLMs[name] = spec
	}

	setBool(&cfg.Proxy.NormalizeToolCalls, "TESTDIAG_PROXY_NORMALIZE_TOOL_CALLS")
	setBool(&cfg.Proxy.InjectTools, "TESTDIAG_PROXY_INJECT_TOOLS")
	setBool(&cfg.Proxy.Debug, "TESTDIAG_PROXY_DEBUG")

	setStr(&cfg.Workspace.Root, "TESTDIAG_WORKSPACE_ROOT")

	setStr(&cfg.Output.Dir, "TESTDIAG_OUTPUT_DIR")

	setInt(&cfg.Diagnosis.MaxAttempts, "TESTDIAG_MAX_ATTEMPTS")
	setInt(&cfg.Diagnosis.MaxToolIterations, "TESTDIAG_MAX_TOOL_ITERATIONS")
	setInt(&cfg.Diagnosis.MaxLogParseFeedbacks, "TESTDIAG_MAX_LOGPARSE_FEEDBACKS")
}

// envName upper-cases an LLM name and replaces any non-alphanumeric run with a
// single underscore so it can appear in an environment variable name.
func envName(name string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		case !prevUnderscore:
			b.WriteRune('_')
			prevUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

// requiredStages are the stages that must be assigned for a run to proceed.
var requiredStages = []string{StageLogParse, StageDeepInspect}

func (c *Config) validate() error {
	if len(c.LLMs) == 0 {
		return fmt.Errorf("no LLMs defined: add at least one [llms.<name>] with base_url and model")
	}
	for name, spec := range c.LLMs {
		if spec.Model == "" {
			return fmt.Errorf("llms.%s.model is required", name)
		}
		if spec.BaseURL == "" {
			return fmt.Errorf("llms.%s.base_url is required for an OpenAI-compatible endpoint", name)
		}
	}
	for _, stage := range requiredStages {
		if _, err := c.LLMForStage(stage); err != nil {
			return err
		}
	}
	if c.Diagnosis.MaxAttempts < 1 {
		c.Diagnosis.MaxAttempts = 1
	}
	if c.Diagnosis.MaxToolIterations < 1 {
		c.Diagnosis.MaxToolIterations = 50
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
