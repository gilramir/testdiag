// Package planner implements the PLAN stage: for each hypothesis it runs a
// lightweight tool-using agent that surveys the workspace and produces an
// annotated list of the most relevant files for DEEPINSPECT to examine.
//
// PLAN is intentionally shallow — breadth over depth. It locates relevant
// files; it does NOT attempt to confirm or refute the hypothesis. That is
// DEEPINSPECT's job.
package planner

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/mapping"
	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// PlanInput carries everything a single PLAN attempt needs.
type PlanInput struct {
	Test            jenkins.FailedTest
	Brief           string // LOGPARSE handoff (no raw log)
	Hypothesis      string // full hypothesis text
	HypothesisIndex int    // 1-based
	ArchDoc         string // optional architecture document
	PrevResult      string // empty on first attempt; prior plan for retry
	Critique        string // empty on first attempt; feedback for retry
}

// Result is the outcome of one PLAN attempt.
type Result struct {
	Content     string   // annotated file list as Markdown
	ToolsCalled []string
}

// Planner runs the PLAN stage against a fixed workspace.
type Planner struct {
	ws                *workspace.Workspace
	llm               config.LLMSpec
	background        string
	memory            string // contents of .testdiag/memory.md (may be empty)
	maxToolIterations int
	mapper            string
}

// New creates a Planner. background is the contents of TEST_AGENT.md (may be
// empty); memory is the contents of .testdiag/memory.md (may be empty);
// mapper is the optional path to the test→source mapping executable.
func New(ws *workspace.Workspace, llm config.LLMSpec, background, memory string, maxToolIterations int, mapper string) *Planner {
	return &Planner{
		ws: ws, llm: llm, background: background, memory: memory,
		maxToolIterations: maxToolIterations, mapper: mapper,
	}
}

// Plan runs one PLAN attempt for one hypothesis. When input.PrevResult and
// input.Critique are set this is a feedback-triggered retry.
func (p *Planner) Plan(ctx context.Context, input PlanInput) (Result, error) {
	m, err := mapping.MapTestToSource(p.mapper, p.ws.Root(), input.Test)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: mapper failed for %s: %v\n", input.Test.FullName(), err)
		m = mapping.Result{}
	}

	// PLAN works from the brief, not the raw failure log.
	tools.SetLogToolsEnabled(false)
	defer tools.SetLogToolsEnabled(true)

	agent, err := p.buildAgent(input)
	if err != nil {
		return Result{}, fmt.Errorf("building plan agent for %s: %w", input.Test.FullName(), err)
	}

	tools.ResetLoopGuard()
	tools.ResetSearchCache()
	tools.ResetFindFilesCache()
	r, err := agent.Run(ctx, buildUserPrompt(input, m, p.background, p.memory))
	if err != nil {
		return Result{}, fmt.Errorf("plan agent run for %s: %w", input.Test.FullName(), err)
	}

	return Result{
		Content:     r.Content,
		ToolsCalled: uniqueToolNames(r),
	}, nil
}

func (p *Planner) buildAgent(input PlanInput) (vnext.Agent, error) {
	name := fmt.Sprintf("plan-%s-h%d", sanitize(input.Test.FullName()), input.HypothesisIndex)
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: buildSystemPrompt(input.Brief, input.Hypothesis),
			LLM: vnext.LLMConfig{
				Provider:    p.llm.Provider,
				Model:       p.llm.Model,
				BaseURL:     p.llm.BaseURL,
				APIKey:      p.llm.APIKey,
				Temperature: p.llm.Temperature,
				MaxTokens:   p.llm.MaxTokens,
			},
			Tools: &vnext.ToolsConfig{
				Enabled: true,
				Reasoning: &vnext.ReasoningConfig{
					Enabled:           true,
					MaxIterations:     p.maxToolIterations,
					ContinueOnToolUse: true,
				},
			},
			Memory:  &vnext.MemoryConfig{Enabled: false},
			Timeout: 10 * time.Minute,
		}).
		Build()
}

// systemPromptBase is the static part of the PLAN system prompt. The brief and
// hypothesis are appended so they survive AGK's continuation loop.
const systemPromptBase = `You are an expert software engineer acting as a CODE NAVIGATOR. Your job is NOT to investigate deeply or prove a hypothesis — the next stage (DEEPINSPECT) will do that. Your job is to PLAN the investigation: given the hypothesis (including the key symbols and files-to-inspect it names), survey the workspace with your tools and produce a prioritized list of concrete (file-path or glob, search pattern, reason) tuples for DEEPINSPECT to follow.

You are given:
- An INVESTIGATION BRIEF from an earlier log-analysis stage (no raw log is available)
- A SPECIFIC HYPOTHESIS to plan around, which names key symbols and a suggested file list

GUIDANCE:
- Start from the key symbols and files named in the hypothesis; use file_exists to verify they exist before listing or reading them.
- Use function_lookup(language, function_name, directories) to find where a named function is defined without writing a regex; it returns file + line number directly.
- Use find_files and search_repo to locate relevant files by name, pattern, or content.
- Use list_directory, grep, and read_lines to quickly confirm a file is relevant — do NOT read entire files.
- Do NOT repeat a search you already performed; each tool call must add new information.
- Aim for BREADTH: identify which files matter, not what they contain.
- Stop when you have a good candidate list (10–12 files maximum).
- Workspace-relative paths only.

When done, output ONLY Markdown in exactly this format:

## Inspection Plan for Hypothesis N: <title>

### High Priority
- ` + "`path/to/file`" + ` — pattern/glob to search or read, and why this file is critical for confirming or refuting the hypothesis

### Medium Priority
- ` + "`path/to/file`" + ` — pattern/glob and reason

### Low Priority
- ` + "`path/to/file`" + ` — pattern/glob and reason (examine if time permits)

Omit sections that have no entries.`

func buildSystemPrompt(brief, hypothesis string) string {
	var b strings.Builder
	b.WriteString(systemPromptBase)
	if strings.TrimSpace(brief) != "" {
		b.WriteString("\n\n## Investigation brief (from LOGPARSE)\n")
		b.WriteString(strings.TrimSpace(brief))
	}
	if strings.TrimSpace(hypothesis) != "" {
		b.WriteString("\n\n## Hypothesis to plan around\n")
		b.WriteString(strings.TrimSpace(hypothesis))
	}
	return b.String()
}

func buildUserPrompt(input PlanInput, m mapping.Result, background, memory string) string {
	var b strings.Builder
	if input.PrevResult != "" {
		fmt.Fprintf(&b, "Your previous inspection plan for hypothesis %d was reviewed and found insufficient.\n\n", input.HypothesisIndex)
		b.WriteString("## What needs to be fixed\n\n")
		b.WriteString(strings.TrimSpace(input.Critique))
		b.WriteString("\n\n## Your previous plan (for reference)\n\n")
		b.WriteString(strings.TrimSpace(input.PrevResult))
		b.WriteString("\n\n")
	} else {
		fmt.Fprintf(&b, "Produce an inspection plan for hypothesis %d.\n\n", input.HypothesisIndex)
	}

	b.WriteString("## Failing test\n")
	fmt.Fprintf(&b, "- Name: %s\n", input.Test.FullName())
	if m.SourceFile != "" {
		fmt.Fprintf(&b, "- Likely source file: %s\n", m.SourceFile)
	}
	b.WriteString("\n")

	if strings.TrimSpace(memory) != "" {
		b.WriteString("## Prior codebase knowledge (from past investigations)\n\n")
		b.WriteString(strings.TrimSpace(memory))
		b.WriteString("\n\n")
	}

	if strings.TrimSpace(background) != "" {
		b.WriteString("## Project background\n\n")
		b.WriteString(strings.TrimSpace(background))
		b.WriteString("\n\n")
	}

	if strings.TrimSpace(input.ArchDoc) != "" {
		b.WriteString("## Architecture document\n\n")
		b.WriteString(strings.TrimSpace(input.ArchDoc))
		b.WriteString("\n\n")
	}

	b.WriteString("Survey the workspace and produce the inspection plan in the required Markdown format.")
	return b.String()
}

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
