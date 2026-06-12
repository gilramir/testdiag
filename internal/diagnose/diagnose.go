// Package diagnose runs a single failing test through an AgenticGoKit agent
// that uses the provider's native tool-calling loop to read workspace files and
// determine the root cause.
package diagnose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/mapping"
	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// logDir is where fetched failure logs are written, relative to the workspace
// root, so the jailed file tools can read them.
const logDir = ".testdiag/logs"

// maxToolIterations caps the native tool-calling loop per test. It is generous
// because a flaky failure often requires tracing across the Python client / C++
// server boundary, which takes many reads.
const maxToolIterations = 30

// Result is the outcome of diagnosing one test.
type Result struct {
	Test        jenkins.FailedTest
	Mapping     mapping.Result
	LogPath     string   // workspace-relative path to the saved log
	RootCause   string   // the agent's Markdown analysis
	ToolsCalled []string // tools the agent invoked (for the report footer)
	Duration    time.Duration
}

// Diagnoser diagnoses tests against a fixed workspace and LLM config.
type Diagnoser struct {
	cfg        *config.Config
	ws         *workspace.Workspace
	background string // contents of TEST_AGENT.md
}

// New creates a Diagnoser. background is the TEST_AGENT.md content (may be "").
func New(cfg *config.Config, ws *workspace.Workspace, background string) *Diagnoser {
	return &Diagnoser{cfg: cfg, ws: ws, background: background}
}

// Diagnose maps the test to its source, persists its log, builds a fresh agent
// (per-test independence), and runs the native tool-calling loop to completion.
func (d *Diagnoser) Diagnose(ctx context.Context, test jenkins.FailedTest) (Result, error) {
	start := time.Now()

	m, err := mapping.MapTestToSource(d.ws.Root(), test)
	if err != nil {
		return Result{}, fmt.Errorf("mapping %s: %w", test.FullName(), err)
	}

	logRel, err := d.saveLog(test)
	if err != nil {
		return Result{}, fmt.Errorf("saving log for %s: %w", test.FullName(), err)
	}

	agent, err := d.buildAgent(test)
	if err != nil {
		return Result{}, fmt.Errorf("building agent for %s: %w", test.FullName(), err)
	}

	excerpt := makeExcerpt(combinedLog(test))
	basePrompt := buildUserPrompt(test, m, logRel, excerpt, d.background)

	// Critique/revise loop: run the agent, and if the draft looks shallow (didn't
	// open source, or never names a flakiness mechanism) re-run it with the gaps
	// fed back, up to MaxAttempts. Each run is independent (memory disabled), so
	// the retry prompt carries the prior draft and the full task forward.
	attempts := d.cfg.Diagnosis.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	prompt := basePrompt
	var (
		res         *vnext.Result
		toolsCalled []string
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		// Scope loop detection to this attempt: each Run is an independent
		// tool-calling loop, so a repeated call only signals a stuck loop within
		// the same run, not across attempts (which use different prompts).
		tools.ResetLoopGuard()
		r, err := agent.Run(ctx, prompt)
		if err != nil {
			return Result{}, fmt.Errorf("agent run for %s: %w", test.FullName(), err)
		}
		res = r
		toolsCalled = append(toolsCalled, r.ToolsCalled...)

		issues := critique(r, logRel)
		if len(issues) == 0 || attempt == attempts {
			break
		}
		fmt.Fprintf(os.Stderr, "  ↻ %s: attempt %d was shallow, re-diagnosing (%s)\n",
			test.FullName(), attempt, strings.Join(issues, "; "))
		prompt = buildRetryPrompt(basePrompt, r.Content, issues)
	}

	return Result{
		Test:        test,
		Mapping:     m,
		LogPath:     logRel,
		RootCause:   res.Content,
		ToolsCalled: toolsCalled,
		Duration:    time.Since(start),
	}, nil
}

// minToolCalls is the fewest tool calls below which we assume the agent barely
// looked at the system before concluding.
const minToolCalls = 3

// critique returns the reasons a diagnosis looks shallow, or nil if it passes.
// It is a cheap, conservative gate for the revise loop: it only flags answers
// that clearly didn't do the work, so a genuinely thorough first attempt is
// accepted without a second (costly) run.
func critique(res *vnext.Result, logPath string) []string {
	var issues []string

	// Did the agent explore source beyond the saved failure log? ToolCalls
	// carries the arguments; fall back to ToolsCalled (names only) if it's empty.
	total := len(res.ToolCalls)
	sourceReads := 0
	for _, c := range res.ToolCalls {
		if p := toolArgPath(c.Arguments); p != "" && p != logPath {
			sourceReads++
		}
	}
	if total == 0 {
		total = len(res.ToolsCalled)
	}
	if total < minToolCalls {
		issues = append(issues, fmt.Sprintf("only %d tool call(s) were made — the system was barely explored", total))
	}
	if len(res.ToolCalls) > 0 && sourceReads == 0 {
		issues = append(issues, "no source files were opened (only the failure log was read) — read the actual client and server code")
	}

	content := strings.ToLower(res.Content)
	if !mentionsMechanism(content) {
		issues = append(issues, "the report names no nondeterminism mechanism (race / timing / ordering / resource / environment) — flaky failures need one")
	}
	if !hasFileCitation(res.Content) {
		issues = append(issues, "the report cites no concrete source file as evidence")
	}
	return issues
}

// mechanismTerms are words that signal the report engaged with WHY a test is
// flaky rather than just what it does.
var mechanismTerms = []string{
	"race", "concurren", "thread", "lock", "mutex", "atomic", "deadlock",
	"timing", "timeout", "deadline", "sleep", "wait", "poll", "async",
	"order", "schedul", "nondetermin", "intermitt", "retry", "backoff",
	"resource", "port", "leak", "limit", "environment", "leftover", "seed",
	"replication", "quorum", "partition", "startup", "ready",
}

func mentionsMechanism(lowerContent string) bool {
	for _, t := range mechanismTerms {
		if strings.Contains(lowerContent, t) {
			return true
		}
	}
	return false
}

// sourceFileRe matches a workspace-relative source path with a recognizable
// extension (Python/C++ and common glue), used to confirm the report cites
// actual code rather than just prose.
var sourceFileRe = regexp.MustCompile(`[\w./-]+\.(py|pyx|cc|cpp|cxx|c|h|hh|hpp|hxx|proto|go|java|rs)\b`)

func hasFileCitation(content string) bool {
	return sourceFileRe.MatchString(content)
}

// toolArgPath extracts a single file path from a tool call's arguments, handling
// both the "path" argument (read_lines/grep/read_file/list_directory) and the
// "paths" list (count_lines).
func toolArgPath(args map[string]interface{}) string {
	if args == nil {
		return ""
	}
	if p, ok := args["path"].(string); ok {
		return p
	}
	switch ps := args["paths"].(type) {
	case string:
		return ps
	case []interface{}:
		if len(ps) > 0 {
			if s, ok := ps[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// buildAgent constructs a fresh agent for one test. Memory is disabled so each
// diagnosis is fully independent; reasoning is enabled so the agent loops:
// call LLM -> execute tools -> feed results back -> repeat.
func (d *Diagnoser) buildAgent(test jenkins.FailedTest) (vnext.Agent, error) {
	name := "diagnose-" + sanitize(test.FullName())
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: systemPrompt,
			LLM: vnext.LLMConfig{
				Provider:    d.cfg.LLM.Provider,
				Model:       d.cfg.LLM.Model,
				BaseURL:     d.cfg.LLM.BaseURL,
				APIKey:      d.cfg.LLM.APIKey,
				Temperature: d.cfg.LLM.Temperature,
				MaxTokens:   d.cfg.LLM.MaxTokens,
			},
			Tools: &vnext.ToolsConfig{
				Enabled: true,
				Reasoning: &vnext.ReasoningConfig{
					Enabled:           true,
					MaxIterations:     maxToolIterations,
					ContinueOnToolUse: true,
				},
			},
			Memory:  &vnext.MemoryConfig{Enabled: false},
			Timeout: 10 * time.Minute,
		}).
		WithPreset(vnext.ChatAgent). // makes the registered internal tools available
		Build()
}

// saveLog writes the test's combined output under <root>/.testdiag/logs and
// returns the workspace-relative path (so the jailed tools can open it).
func (d *Diagnoser) saveLog(test jenkins.FailedTest) (string, error) {
	dir := filepath.Join(d.ws.Root(), logDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	rel := filepath.Join(logDir, sanitize(test.FullName())+".log")
	abs := filepath.Join(d.ws.Root(), rel)
	if err := os.WriteFile(abs, []byte(combinedLog(test)), 0o644); err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// combinedLog assembles the full failure output the way a developer would see
// it: error summary, stack trace, then captured stdout/stderr.
func combinedLog(test jenkins.FailedTest) string {
	var b strings.Builder
	section := func(title, body string) {
		if strings.TrimSpace(body) == "" {
			return
		}
		fmt.Fprintf(&b, "===== %s =====\n%s\n\n", title, strings.TrimRight(body, "\n"))
	}
	section("ERROR DETAILS", test.ErrorDetails)
	section("STACK TRACE", test.ErrorStackTrace)
	section("STDOUT", test.Stdout)
	section("STDERR", test.Stderr)
	if b.Len() == 0 {
		return "(no failure output was provided by Jenkins for this test)\n"
	}
	return b.String()
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
