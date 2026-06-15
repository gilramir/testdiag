// Package config loads testdiag configuration from a TOML file under
// ~/.config/testdiag/config.toml, with environment-variable overrides.
//
// Environment variables always win over the file, so secrets can be injected
// at runtime (e.g. in CI) without writing them to disk.
//
// The workflow is a state machine of stages:
//
//	DOWNLOAD → LOGPARSE → FEEDBACK → HYPOTHESIZE → FEEDBACK →
//	[PLANINSPECTION → FEEDBACK → DEEPINSPECT → FEEDBACK] × N → SUMMARIZE → FEEDBACK → LESSONS
//
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

// Stage names — the keys used in the [stages] table.
const (
	StageLogParse            = "logparse"
	StageLogParseFeedback    = "logparse_feedback"       // optional; falls back to logparse LLM
	StageHypothsize          = "hypothesize"             // optional; falls back to logparse LLM
	StageHypothsizeFeedback  = "hypothesize_feedback"    // optional; falls back to hypothesize LLM
	StagePlanInspect         = "planinspection"          // optional; falls back to deepinspect LLM
	StagePlanInspectFeedback = "planinspection_feedback" // optional; falls back to planinspection LLM
	StageSetGoals            = "setgoals"                // optional; falls back to deepinspect LLM
	StageSetGoalsFeedback    = "setgoals_feedback"       // optional; falls back to setgoals LLM
	StageDeepInspect         = "deepinspect"
	StageDeepInspectFeedback = "deepinspect_feedback" // optional; falls back to deepinspect LLM
	StageSummarize           = "summarize"            // optional; falls back to logparse LLM
	StageSummarizeFeedback   = "summarize_feedback"   // optional; falls back to summarize LLM
	StageLessons             = "lessons"              // optional; falls back to logparse LLM
	StageMemoize             = "memorize"             // optional; falls back to logparse LLM
)

// Config is the fully-resolved configuration for a testdiag run.
type Config struct {
	Jenkins     Jenkins            `toml:"jenkins"`
	LLMs        map[string]LLMSpec `toml:"llms"`   // named LLMs, referenced by stages
	Stages      map[string]string  `toml:"stages"` // stage name -> LLM name
	Workspace   Workspace          `toml:"workspace"`
	Output      Output             `toml:"output"`
	StageConfig StageConfig        `toml:"stage_config"`
}

// Jenkins holds credentials for talking to the Jenkins HTTP API. Jenkins uses
// HTTP Basic auth with the user's login and a personal API token (NOT the
// account password): Authorization: Basic base64(user:apiToken).
type Jenkins struct {
	User     string `toml:"user"`
	APIToken string `toml:"api_token"`
}

// LLMSpec is one named LLM endpoint. Several may be defined and assigned to
// different stages. Each points at any OpenAI-API-compatible server (including
// a local one).
type LLMSpec struct {
	Provider      string  `toml:"provider"`       // "openai" for OpenAI-compatible servers
	BaseURL       string  `toml:"base_url"`       // e.g. http://localhost:1234/v1
	Model         string  `toml:"model"`          // model name the server exposes
	APIKey        string  `toml:"api_key"`        // may be empty for local servers
	ContextWindow int     `toml:"context_window"` // model context size in tokens (0 = unknown)
	Temperature   float32 `toml:"temperature"`    // sampling temperature
	MaxTokens     int     `toml:"max_tokens"`     // max tokens per completion
}

// Workspace describes the local checkout the failing tests came from. The
// file-reading tools are jailed to Root.
type Workspace struct {
	// Root is the absolute path to the git workspace the Jenkins build ran
	// against. If empty, the current working directory is used. TEST_AGENT.md
	// is expected at the root of this directory.
	Root string `toml:"root"`
	// ArchitectureDoc is the workspace-relative path to a document describing
	// the system architecture. HYPOTHESIZE reads it to reason about what
	// components could have caused the failure. Optional — if empty or the file
	// does not exist, HYPOTHESIZE works from the investigation brief alone.
	ArchitectureDoc string `toml:"architecture_doc"`
	// Mapper is the path to an executable that translates a Jenkins test name
	// into a workspace-relative source file path. It is called as:
	//   <mapper> "<test.FullName()>"
	// and must print the source file path on stdout (trailing newline is
	// trimmed). The subprocess runs with the workspace root as its working
	// directory. Optional — if empty, DEEPINSPECT locates the file itself via
	// the directory/grep tools.
	Mapper string `toml:"mapper"`
}

// StageConfig holds per-stage tuning knobs. Each field controls a specific
// aspect of one pipeline stage; zero values use the built-in defaults.
//
// In config.toml:
//
//	[stage_config]
//	logparse_max_feedbacks = 2
//	hypothesize_max_feedbacks = 2
//	deepinspect_max_feedbacks = 1
//	deepinspect_max_tool_iterations = 50
//	summarize_max_feedbacks = 2
type StageConfig struct {
	// LogParseMaxFeedbacks is the number of times the FEEDBACK stage may reject
	// a LOGPARSE brief before the test is abandoned. 0 disables LOGPARSE feedback.
	LogParseMaxFeedbacks int `toml:"logparse_max_feedbacks"`
	// HypothesizeMaxFeedbacks is the number of times the FEEDBACK stage may
	// reject a HYPOTHESIZE output. 0 disables HYPOTHESIZE feedback.
	HypothesizeMaxFeedbacks int `toml:"hypothesize_max_feedbacks"`
	// PlanMaxFeedbacks is the number of times the FEEDBACK stage may reject a
	// PLANINSPECTION output for one hypothesis before marking it as failed
	// (soft) and moving on. 0 disables PLANINSPECTION feedback.
	PlanMaxFeedbacks int `toml:"planinspection_max_feedbacks"`
	// PlanMaxToolIterations caps the tool-calling loop within a single
	// PLANINSPECTION attempt. PLANINSPECTION is a breadth-first survey, so
	// this is intentionally lower than the DEEPINSPECT budget.
	PlanMaxToolIterations int `toml:"planinspection_max_tool_iterations"`
	// SetGoalsMaxFeedbacks is the number of times the FEEDBACK stage may reject
	// a SETGOALS output for one hypothesis before marking it as failed (soft)
	// and moving on. 0 disables SETGOALS feedback.
	SetGoalsMaxFeedbacks int `toml:"setgoals_max_feedbacks"`
	// DeepInspectMaxFeedbacks is the number of times the FEEDBACK stage may
	// reject a DEEPINSPECT result for one hypothesis before marking it as
	// failed (and moving on to the next hypothesis). 0 disables DEEPINSPECT
	// feedback.
	DeepInspectMaxFeedbacks int `toml:"deepinspect_max_feedbacks"`
	// DeepInspectMaxToolIterations caps the tool-calling loop within a single
	// DEEPINSPECT attempt: how many times the agent may call a tool and feed
	// the result back before it must produce an answer.
	DeepInspectMaxToolIterations int `toml:"deepinspect_max_tool_iterations"`
	// SummarizeMaxFeedbacks is the number of times the FEEDBACK stage may reject
	// the SUMMARIZE output. 0 disables SUMMARIZE feedback.
	SummarizeMaxFeedbacks int `toml:"summarize_max_feedbacks"`
	// InspectMaxKnowledgeChars caps the size, in characters, of the accumulated
	// knowledge tree rendered into the PLANINSPECTION/DEEPINSPECT context each
	// turn. Above this, least-recently-referenced facts are evicted (file line
	// text first, then whole records). 0 means unlimited.
	InspectMaxKnowledgeChars int `toml:"inspect_max_knowledge_chars"`
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
// fallback. Use this for optional stages like hypothesize, summarize, and the
// per-stage feedback overrides.
func (c *Config) LLMForStageOptional(stage string) (LLMSpec, bool) {
	name, ok := c.Stages[stage]
	if !ok || name == "" {
		return LLMSpec{}, false
	}
	spec, ok := c.LLMs[name]
	return spec, ok
}

// UserConfigPath returns the user-level config file location
// (~/.config/testdiag/config.toml on Linux).
func UserConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "testdiag", "config.toml"), nil
}

// Load reads configuration from two sources in order, with later sources
// overriding earlier ones, then applies environment-variable overrides:
//
//  1. <workspace>/testdiag.toml  — project config, checked in with the repo
//  2. ~/.config/testdiag/config.toml — user config (API keys, personal prefs)
//  3. TESTDIAG_* environment variables — always win, for CI secrets
//
// The workspace root used to locate testdiag.toml is resolved before any
// config file is read: TESTDIAG_WORKSPACE_ROOT if set, otherwise the nearest
// ancestor directory that contains a .git entry (walking up from CWD), falling
// back to CWD itself. This bootstrap root also becomes the final workspace.root
// when neither config file sets it explicitly.
func Load() (*Config, error) {
	cfg := defaults()

	wsRoot := bootstrapWorkspaceRoot()

	wsConfig := filepath.Join(wsRoot, "testdiag.toml")
	if err := loadRequired(wsConfig, cfg); err != nil {
		return nil, err
	}

	userPath, err := UserConfigPath()
	if err != nil {
		return nil, err
	}
	if err := loadIfExists(userPath, cfg); err != nil {
		return nil, err
	}

	applyEnvOverrides(cfg)

	if cfg.Workspace.Root == "" {
		cfg.Workspace.Root = wsRoot
	}
	if abs, err := filepath.Abs(cfg.Workspace.Root); err == nil {
		cfg.Workspace.Root = abs
	}

	cfg.normalizeLLMs()
	return cfg, cfg.validate()
}

// bootstrapWorkspaceRoot returns the workspace root to use when locating
// testdiag.toml, before any config file has been read:
//  1. TESTDIAG_WORKSPACE_ROOT env var if set
//  2. Nearest ancestor of CWD that contains a .git entry
//  3. CWD itself
func bootstrapWorkspaceRoot() string {
	if v := os.Getenv("TESTDIAG_WORKSPACE_ROOT"); v != "" {
		return v
	}
	if root, ok := findGitRoot(); ok {
		return root
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// findGitRoot walks up from CWD looking for a directory that contains .git.
// Returns the directory and true on success, or ("", false) if none is found.
func findGitRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // reached filesystem root
		}
		dir = parent
	}
}

// loadRequired decodes path into cfg. A missing file is an error.
func loadRequired(path string, cfg *Config) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("workspace config not found: %s", path)
		}
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

// loadIfExists decodes path into cfg when the file exists. A missing file is
// not an error. Any other stat or parse error is returned.
func loadIfExists(path string, cfg *Config) error {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

func defaults() *Config {
	return &Config{
		Output: Output{
			Dir: "test-diagnosis",
		},
		StageConfig: StageConfig{
			LogParseMaxFeedbacks:         2,
			HypothesizeMaxFeedbacks:      2,
			PlanMaxFeedbacks:             1,
			PlanMaxToolIterations:        20,
			SetGoalsMaxFeedbacks:         2,
			DeepInspectMaxFeedbacks:      1,
			DeepInspectMaxToolIterations: 50,
			SummarizeMaxFeedbacks:        2,
			InspectMaxKnowledgeChars:     24000,
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

	setStr(&cfg.Workspace.Root, "TESTDIAG_WORKSPACE_ROOT")
	setStr(&cfg.Workspace.ArchitectureDoc, "TESTDIAG_ARCHITECTURE_DOC")
	setStr(&cfg.Workspace.Mapper, "TESTDIAG_MAPPER")

	setStr(&cfg.Output.Dir, "TESTDIAG_OUTPUT_DIR")

	setInt(&cfg.StageConfig.LogParseMaxFeedbacks, "TESTDIAG_LOGPARSE_MAX_FEEDBACKS")
	setInt(&cfg.StageConfig.HypothesizeMaxFeedbacks, "TESTDIAG_HYPOTHESIZE_MAX_FEEDBACKS")
	setInt(&cfg.StageConfig.PlanMaxFeedbacks, "TESTDIAG_PLANINSPECTION_MAX_FEEDBACKS")
	setInt(&cfg.StageConfig.PlanMaxToolIterations, "TESTDIAG_PLANINSPECTION_MAX_TOOL_ITERATIONS")
	setInt(&cfg.StageConfig.SetGoalsMaxFeedbacks, "TESTDIAG_SETGOALS_MAX_FEEDBACKS")
	setInt(&cfg.StageConfig.DeepInspectMaxFeedbacks, "TESTDIAG_DEEPINSPECT_MAX_FEEDBACKS")
	setInt(&cfg.StageConfig.DeepInspectMaxToolIterations, "TESTDIAG_DEEPINSPECT_MAX_TOOL_ITERATIONS")
	setInt(&cfg.StageConfig.SummarizeMaxFeedbacks, "TESTDIAG_SUMMARIZE_MAX_FEEDBACKS")
	setInt(&cfg.StageConfig.InspectMaxKnowledgeChars, "TESTDIAG_INSPECT_MAX_KNOWLEDGE_CHARS")
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
	if cfg := &c.StageConfig; cfg.DeepInspectMaxToolIterations < 1 {
		cfg.DeepInspectMaxToolIterations = 50
	}
	if cfg := &c.StageConfig; cfg.PlanMaxToolIterations < 1 {
		cfg.PlanMaxToolIterations = 20
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
