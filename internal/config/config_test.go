package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestExampleConfigValidates keeps config.example.toml in sync with the schema:
// it must decode and pass the same normalization + validation a real run does.
func TestExampleConfigValidates(t *testing.T) {
	cfg := defaults()
	if _, err := toml.DecodeFile("../../config.example.toml", cfg); err != nil {
		t.Fatalf("decoding example config: %v", err)
	}
	cfg.normalizeLLMs()
	if err := cfg.validate(); err != nil {
		t.Fatalf("example config fails validation: %v", err)
	}
	for _, stage := range requiredStages {
		if _, err := cfg.LLMForStage(stage); err != nil {
			t.Errorf("example config: %v", err)
		}
	}
}

// twoStageConfig returns a minimal valid config: two named LLMs and both
// required stages assigned.
func twoStageConfig() *Config {
	return &Config{
		LLMs: map[string]LLMSpec{
			"fast": {Provider: "openai", BaseURL: "http://fast/v1", Model: "fast-model"},
			"deep": {Provider: "openai", BaseURL: "http://deep/v1", Model: "deep-model"},
		},
		Stages: map[string]string{
			StageLogParse:    "fast",
			StageDeepInspect: "deep",
		},
	}
}

func TestLLMForStage(t *testing.T) {
	cfg := twoStageConfig()

	spec, err := cfg.LLMForStage(StageDeepInspect)
	if err != nil {
		t.Fatalf("LLMForStage(deepinspect): %v", err)
	}
	if spec.Model != "deep-model" {
		t.Errorf("got model %q, want deep-model", spec.Model)
	}

	// Unassigned stage.
	if _, err := cfg.LLMForStage("nonexistent"); err == nil {
		t.Error("expected error for unassigned stage, got nil")
	}

	// Assigned to an undefined LLM.
	cfg.Stages[StageLogParse] = "missing"
	if _, err := cfg.LLMForStage(StageLogParse); err == nil {
		t.Error("expected error for stage referencing undefined LLM, got nil")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(*Config) {}, false},
		{"no llms", func(c *Config) { c.LLMs = nil }, true},
		{"missing model", func(c *Config) {
			s := c.LLMs["fast"]
			s.Model = ""
			c.LLMs["fast"] = s
		}, true},
		{"missing base_url", func(c *Config) {
			s := c.LLMs["deep"]
			s.BaseURL = ""
			c.LLMs["deep"] = s
		}, true},
		{"stage unassigned", func(c *Config) { delete(c.Stages, StageDeepInspect) }, true},
		{"stage references undefined llm", func(c *Config) { c.Stages[StageDeepInspect] = "ghost" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := twoStageConfig()
			tt.mutate(cfg)
			err := cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeLLMs(t *testing.T) {
	cfg := &Config{LLMs: map[string]LLMSpec{
		"x": {BaseURL: "http://x/v1", Model: "m"}, // no provider, no max_tokens
	}}
	cfg.normalizeLLMs()
	got := cfg.LLMs["x"]
	if got.Provider != "openai" {
		t.Errorf("provider default = %q, want openai", got.Provider)
	}
	if got.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens default = %d, want %d", got.MaxTokens, defaultMaxTokens)
	}
}

func TestEnvName(t *testing.T) {
	cases := map[string]string{
		"fast":     "FAST",
		"deep-llm": "DEEP_LLM",
		"gpt.oss":  "GPT_OSS",
		"a  b":     "A_B",
		"_weird_":  "WEIRD",
		"qwen2.5":  "QWEN2_5",
	}
	for in, want := range cases {
		if got := envName(in); got != want {
			t.Errorf("envName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadRequired(t *testing.T) {
	t.Run("missing file returns error", func(t *testing.T) {
		err := loadRequired(filepath.Join(t.TempDir(), "testdiag.toml"), defaults())
		if err == nil {
			t.Fatal("expected error for missing file, got nil")
		}
		if !strings.Contains(err.Error(), "workspace config not found") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("present file is parsed", func(t *testing.T) {
		dir := t.TempDir()
		content := `[llms.x]
base_url = "http://x/v1"
model = "m"
`
		if err := os.WriteFile(filepath.Join(dir, "testdiag.toml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		cfg := defaults()
		if err := loadRequired(filepath.Join(dir, "testdiag.toml"), cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := cfg.LLMs["x"]; !ok {
			t.Error("expected llm 'x' to be parsed")
		}
	})
}

func TestApplyEnvOverridesNamedLLM(t *testing.T) {
	cfg := twoStageConfig()
	t.Setenv("TESTDIAG_LLM_FAST_API_KEY", "secret-key")
	t.Setenv("TESTDIAG_LLM_DEEP_BASE_URL", "http://override/v1")

	applyEnvOverrides(cfg)

	if got := cfg.LLMs["fast"].APIKey; got != "secret-key" {
		t.Errorf("fast api_key = %q, want secret-key", got)
	}
	if got := cfg.LLMs["deep"].BaseURL; got != "http://override/v1" {
		t.Errorf("deep base_url = %q, want override", got)
	}
}
