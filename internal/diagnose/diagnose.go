// Package diagnose implements the DEEPINSPECT stage: it runs a single failing
// test through an AgenticGoKit agent that uses the provider's native
// tool-calling loop to read workspace SOURCE files and determine the root cause.
//
// Each call to Diagnose runs one attempt for one hypothesis. The feedback/retry
// loop is managed externally by the pipeline (deepInspectAllStage + feedback
// stage), so this package is a pure "run one agent, return the result" layer.
//
// The agent is NOT given the raw Jenkins log. It works from the investigation
// brief produced by LOGPARSE and the specific hypothesis from HYPOTHESIZE. The
// raw-log tools are hard-disabled for the duration of the run.
package diagnose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/mapping"
	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// notebookDir is where each test's investigation notebook lives, relative to
// the workspace root, so the notebook tool (jailed like the rest) can r/w it.
const notebookDir = ".testdiag/notes"

// DiagnoseInput carries everything a single DEEPINSPECT attempt needs.
type DiagnoseInput struct {
	Test            jenkins.FailedTest
	Brief           string // LOGPARSE handoff (not the raw log)
	Hypothesis      string // the full hypothesis text to investigate
	HypothesisIndex int    // 1-based index for notebook naming
	Plan            string // PLAN output: annotated file list (may be empty)
	PrevResult      string // empty on first attempt; prior draft for retry
	Critique        string // empty on first attempt; feedback for retry
}

// Result is the outcome of one DEEPINSPECT attempt.
type Result struct {
	Content     string   // the agent's Markdown analysis
	ToolsCalled []string // tools the agent invoked (for reporting)
}

// Diagnoser runs the DEEPINSPECT stage against a fixed workspace using the LLM
// assigned to that stage.
type Diagnoser struct {
	ws                *workspace.Workspace
	llm               config.LLMSpec
	background        string // contents of TEST_AGENT.md
	memory            string // contents of .testdiag/memory.md (may be empty)
	maxToolIterations int
	mapper            string // path to test→source mapper executable; may be empty
	drainFn           func() // called at the start of each Diagnose(); may be nil
}

// New creates a Diagnoser. llm is the LLM assigned to the DEEPINSPECT stage;
// background is the TEST_AGENT.md content (may be "");
// memory is the contents of .testdiag/memory.md (may be "");
// maxToolIterations caps the tool-calling loop per attempt;
// mapper is the optional path to the test→source mapping executable;
// drainFn, if non-nil, is called before each attempt to discard any queued
// operator messages that arrived between hypothesis runs.
func New(ws *workspace.Workspace, llm config.LLMSpec, background, memory string, maxToolIterations int, mapper string, drainFn func()) *Diagnoser {
	return &Diagnoser{
		ws: ws, llm: llm, background: background, memory: memory,
		maxToolIterations: maxToolIterations, mapper: mapper,
		drainFn: drainFn,
	}
}

// Diagnose runs one DEEPINSPECT attempt for one hypothesis. When input.PrevResult
// and input.Critique are non-empty this is a feedback-triggered retry: the
// previous draft and the feedback are included in the user message so the agent
// knows exactly what to improve. Each call builds a fresh agent (memory
// disabled), so prior runs have no effect on this one.
func (d *Diagnoser) Diagnose(ctx context.Context, input DiagnoseInput) (Result, error) {
	if d.drainFn != nil {
		d.drainFn()
	}

	m, err := mapping.MapTestToSource(d.mapper, d.ws.Root(), input.Test)
	if err != nil {
		// Mapper failure is non-fatal: warn and let the agent find the file itself.
		fmt.Fprintf(os.Stderr, "warning: mapper failed for %s: %v\n", input.Test.FullName(), err)
		m = mapping.Result{}
	}

	if _, err := d.prepareNotebook(input.Test, input.HypothesisIndex); err != nil {
		return Result{}, fmt.Errorf("preparing notebook for %s: %w", input.Test.FullName(), err)
	}

	// Hard-block the raw failure log: DEEPINSPECT works only from the brief.
	tools.SetLogToolsEnabled(false)
	defer tools.SetLogToolsEnabled(true)

	agent, err := d.buildAgent(input)
	if err != nil {
		return Result{}, fmt.Errorf("building agent for %s: %w", input.Test.FullName(), err)
	}

	tools.ResetLoopGuard()
	tools.ResetSearchCache()
	tools.ResetFindFilesCache()
	r, err := agent.Run(ctx, buildUserPrompt(input, m, d.background, d.memory))
	if err != nil {
		return Result{}, fmt.Errorf("agent run for %s: %w", input.Test.FullName(), err)
	}

	return Result{
		Content:     r.Content,
		ToolsCalled: uniqueToolNames(r),
	}, nil
}

// uniqueToolNames merges r.ToolsCalled with the names from r.ToolCalls,
// de-duplicating while preserving order.
func uniqueToolNames(r *vnext.Result) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, n := range r.ToolsCalled {
		add(n)
	}
	for _, c := range r.ToolCalls {
		add(c.Name)
	}
	return out
}

// buildAgent constructs a fresh agent for one hypothesis. Memory is disabled
// so each attempt is fully independent. We do NOT apply a builder preset: the
// registered internal tools attach via Tools.Enabled alone (DiscoverInternalTools),
// and presets would clobber SystemPrompt/Temperature and re-enable memory.
//
// The brief and hypothesis are in the system prompt (not only the user message)
// because AGK's continuation loop preserves System across every tool iteration
// but replaces User with "Previous response + tool results" after the first
// round-trip.
func (d *Diagnoser) buildAgent(input DiagnoseInput) (vnext.Agent, error) {
	name := fmt.Sprintf("diagnose-%s-h%d", sanitize(input.Test.FullName()), input.HypothesisIndex)
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: buildSystemPrompt(input.Brief, input.Hypothesis, d.maxToolIterations),
			LLM: vnext.LLMConfig{
				Provider:    d.llm.Provider,
				Model:       d.llm.Model,
				BaseURL:     d.llm.BaseURL,
				APIKey:      d.llm.APIKey,
				Temperature: d.llm.Temperature,
				MaxTokens:   d.llm.MaxTokens,
			},
			Tools: &vnext.ToolsConfig{
				Enabled: true,
				Reasoning: &vnext.ReasoningConfig{
					Enabled:           true,
					MaxIterations:     d.maxToolIterations,
					ContinueOnToolUse: true,
				},
			},
			Memory:  &vnext.MemoryConfig{Enabled: false},
			Timeout: 10 * time.Minute,
		}).
		Build()
}

// prepareNotebook starts a fresh per-hypothesis notebook under .testdiag/notes/
// and points the notebook tool at it. Each hypothesis gets its own file so
// concurrent read-backs don't bleed across investigations.
func (d *Diagnoser) prepareNotebook(test jenkins.FailedTest, hypothesisIdx int) (string, error) {
	dir := filepath.Join(d.ws.Root(), notebookDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := sanitize(test.FullName())
	if hypothesisIdx > 0 {
		base = fmt.Sprintf("%s.h%d", base, hypothesisIdx)
	}
	rel := filepath.Join(notebookDir, base+".md")
	abs := filepath.Join(d.ws.Root(), rel)
	header := fmt.Sprintf("# Investigation notebook: %s (hypothesis %d)\n\n", test.FullName(), hypothesisIdx)
	if err := os.WriteFile(abs, []byte(header), 0o644); err != nil {
		return "", err
	}
	relSlash := filepath.ToSlash(rel)
	tools.SetNotebookPath(relSlash)
	return relSlash, nil
}

// sanitize makes a test name safe to use as a single filename segment.
func sanitize(s string) string {
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}
	out := strings.Map(repl, s)
	if len(out) > 180 {
		out = out[:180]
	}
	if out == "" {
		return "test"
	}
	return out
}
