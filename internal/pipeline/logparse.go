package pipeline

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
	"github.com/gilbertr/testdiag/internal/workspace"
)

// handoffDir holds the Markdown handoff files stages write for the next stage,
// relative to the workspace root.
const handoffDir = ".testdiag/handoff"

// logParseStage runs ONE LLM pass over the raw failure log and distills it into
// an investigation brief for DEEPINSPECT. It uses no tools — the whole log
// (excerpted to the model's context window) is given inline.
type logParseStage struct {
	ws  *workspace.Workspace
	llm config.LLMSpec
}

func newLogParseStage(ws *workspace.Workspace, llm config.LLMSpec) *logParseStage {
	return &logParseStage{ws: ws, llm: llm}
}

func (s *logParseStage) Name() State { return StateLogParse }

func (s *logParseStage) Run(ctx context.Context, sc *Context) error {
	agent, err := s.buildAgent(sc.Test)
	if err != nil {
		return fmt.Errorf("building agent: %w", err)
	}
	head, tail := s.excerptHeadTail()
	excerpt := makeExcerpt(combinedLog(sc.Test), head, tail)
	r, err := agent.Run(ctx, buildLogParsePrompt(sc.Test, excerpt))
	if err != nil {
		return fmt.Errorf("agent run: %w", err)
	}
	if strings.TrimSpace(r.Content) == "" {
		return fmt.Errorf("agent returned empty brief for %s", sc.Test.FullName())
	}
	rel, err := s.writeBrief(sc.Test, r.Content)
	if err != nil {
		return err
	}
	sc.LogParsePath = rel
	sc.Brief = r.Content
	return nil
}

// buildAgent constructs a tool-less, memoryless single-pass agent on the
// LOGPARSE LLM. No builder preset is applied: a preset would clobber the system
// prompt and re-enable memory; we want neither (see diagnose.buildAgent).
func (s *logParseStage) buildAgent(test jenkins.FailedTest) (vnext.Agent, error) {
	name := "logparse-" + sanitize(test.FullName())
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: logParseSystemPrompt,
			LLM: vnext.LLMConfig{
				Provider:    s.llm.Provider,
				Model:       s.llm.Model,
				BaseURL:     s.llm.BaseURL,
				APIKey:      s.llm.APIKey,
				Temperature: s.llm.Temperature,
				MaxTokens:   s.llm.MaxTokens,
			},
			Tools:   &vnext.ToolsConfig{Enabled: false},
			Memory:  &vnext.MemoryConfig{Enabled: false},
			Timeout: 10 * time.Minute,
		}).
		Build()
}

// writeBrief saves the brief to .testdiag/handoff/<test>.logparse.md and returns
// the workspace-relative path.
func (s *logParseStage) writeBrief(test jenkins.FailedTest, brief string) (string, error) {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	rel := filepath.Join(handoffDir, sanitize(test.FullName())+".logparse.md")
	abs := filepath.Join(s.ws.Root(), rel)
	header := fmt.Sprintf("# Investigation brief (LOGPARSE): %s\n\n", test.FullName())
	if err := os.WriteFile(abs, []byte(header+strings.TrimSpace(brief)+"\n"), 0o644); err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// excerptHeadTail picks how many head/tail lines of the raw log to inline,
// scaled to the LLM's context window. The full log is always saved to disk; this
// only bounds what we put in the single LOGPARSE prompt so a huge log can't blow
// the window.
func (s *logParseStage) excerptHeadTail() (head, tail int) {
	switch cw := s.llm.ContextWindow; {
	case cw >= 100000:
		return 800, 400
	case cw >= 32000:
		return 400, 200
	case cw > 0:
		return 200, 100
	default:
		return 150, 100 // context window unknown — stay conservative
	}
}

// makeExcerpt returns the head and tail of log joined with an elision marker, so
// very large logs don't blow up the prompt. A log within head+tail lines is
// returned whole.
func makeExcerpt(log string, head, tail int) string {
	lines := strings.Split(log, "\n")
	if len(lines) <= head+tail {
		return log
	}
	omitted := len(lines) - head - tail
	var b strings.Builder
	b.WriteString(strings.Join(lines[:head], "\n"))
	fmt.Fprintf(&b, "\n... [%d lines omitted from the middle of the log] ...\n", omitted)
	b.WriteString(strings.Join(lines[len(lines)-tail:], "\n"))
	return b.String()
}

// logParseSystemPrompt instructs the LOGPARSE model. Its only job is to turn the
// raw failure log into a focused brief for the next stage — NOT to fix anything.
const logParseSystemPrompt = `You are a CI log analyst. You are given the raw failure log of ONE automated test that is FLAKY (it passes on most runs and failed only intermittently). You do NOT have access to the source code and you must NOT guess at fixes.

Your ONLY job is to read the log and produce a concise INVESTIGATION BRIEF that a second engineer — who will NOT see this log, only your brief — can use to go straight into the source code and find the root cause. Extract leads, name names, and point at where to look.

Work from the log only:
- Find the FIRST genuine error / assertion / exception / timeout, not the downstream noise it caused.
- Pull out the concrete identifiers the next stage will need to locate code: file paths, class/function/method names, modules, error messages, log tags, ports, RPC/endpoint names, thread or process names — quote them verbatim from the log.
- Because the test is flaky, hypothesize what could differ between a passing and this failing run: a race, an ordering assumption, a timeout/deadline, a retry, a resource or port collision, leftover state, an environment condition. Tie each hypothesis to specific evidence in the log (a line, a timestamp gap, an ordering, a stack frame).

Output ONLY Markdown with exactly these sections (no preamble, no code fixes):
## First Real Error
The earliest genuine failure, quoted, with the log location/context that identifies it.
## Source/Logic To Find
A bulleted list of the specific files, symbols, and call paths the next stage should open, each with WHY (what to confirm there). Use the exact identifiers from the log.
## Conditions To Check (flakiness hypotheses)
A ranked bulleted list of the nondeterministic conditions that could explain an intermittent failure, each tied to the log evidence that suggests it.
## Notes For Next Stage
Anything else useful: ambiguities, multiple candidate errors, what would confirm or rule out each hypothesis. Keep it short.`

// buildLogParsePrompt assembles the single user message for the LOGPARSE pass.
func buildLogParsePrompt(test jenkins.FailedTest, logExcerpt string) string {
	var b strings.Builder
	b.WriteString("Produce the investigation brief for this failing test.\n\n")
	b.WriteString("## Failing test\n")
	fmt.Fprintf(&b, "- Name: %s\n", test.FullName())
	if test.Status != "" {
		fmt.Fprintf(&b, "- Status: %s\n", test.Status)
	}
	b.WriteString("\n")

	if strings.TrimSpace(test.ErrorDetails) != "" {
		b.WriteString("## Error details\n```\n")
		b.WriteString(strings.TrimSpace(test.ErrorDetails))
		b.WriteString("\n```\n\n")
	}

	b.WriteString("## Failure log\n```\n")
	b.WriteString(logExcerpt)
	b.WriteString("\n```\n\n")
	b.WriteString("Remember: the next engineer will only see your brief, not this log. Name the exact files, symbols, and conditions they should investigate.")
	return b.String()
}
