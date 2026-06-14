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

// summarizeStage reads all hypotheses and DEEPINSPECT results and asks an LLM
// to summarize each hypothesis (noting whether an inspection result is available
// or not) then identify the most likely root cause. A FEEDBACK gate checks the
// output; if rejected, SUMMARIZE retries with the critique, up to maxFeedbacks
// times.
type summarizeStage struct {
	ws           *workspace.Workspace
	llm          config.LLMSpec
	feedback     *feedbackChecker
	maxFeedbacks int
	verbose      bool
	pauseFn      func() // non-nil when -p is set; called after each handoff print
}

func newSummarizeStage(ws *workspace.Workspace, llm config.LLMSpec, fb *feedbackChecker, maxFeedbacks int, verbose bool, pauseFn func()) *summarizeStage {
	return &summarizeStage{ws: ws, llm: llm, feedback: fb, maxFeedbacks: maxFeedbacks, verbose: verbose, pauseFn: pauseFn}
}

func (s *summarizeStage) Name() State { return StateSummarize }

func (s *summarizeStage) Run(ctx context.Context, sc *Context) error {
	var (
		prevOutput string
		critique   string
	)
	for feedbacks := 0; ; {
		stageBanner(s.verbose, string(s.Name()), feedbacks+1)
		agent, err := s.buildAgent(sc.Test)
		if err != nil {
			return fmt.Errorf("building agent: %w", err)
		}
		var prompt string
		if critique == "" {
			prompt = buildSummarizePrompt(sc.Test, sc.Brief, sc.Hypotheses, sc.DeepInspects)
		} else {
			prompt = buildSummarizeRetryPrompt(sc.Test, sc.Brief, sc.Hypotheses, sc.DeepInspects, prevOutput, critique)
		}
		r, err := agent.Run(ctx, prompt)
		if err != nil {
			return fmt.Errorf("agent run: %w", err)
		}
		content := strings.TrimSpace(r.Content)
		if content == "" {
			return fmt.Errorf("SUMMARIZE agent returned empty output for %s", sc.Test.FullName())
		}

		if s.feedback == nil {
			return s.save(sc, content)
		}

		ok, newCritique, err := s.feedback.Check(ctx, sc.Test, content, "")
		if err != nil {
			return fmt.Errorf("feedback: %w", err)
		}
		if s.verbose {
			if ok {
				fmt.Fprintf(os.Stdout, "  SUMMARIZE FEEDBACK: APPROVED\n")
			} else {
				fmt.Fprintf(os.Stdout, "  SUMMARIZE FEEDBACK: NEEDS REVISION: %s\n", newCritique)
			}
		}
		if ok {
			return s.save(sc, content)
		}
		feedbacks++
		if feedbacks >= s.maxFeedbacks {
			return fmt.Errorf("%s: SUMMARIZE did not meet goals after %d feedback(s): %s",
				sc.Test.FullName(), feedbacks, newCritique)
		}
		prevOutput = content
		critique = newCritique
	}
}

func (s *summarizeStage) save(sc *Context, content string) error {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rel := filepath.Join(handoffDir, sanitize(sc.Test.FullName())+".summarize.md")
	abs := filepath.Join(s.ws.Root(), rel)
	header := fmt.Sprintf("# Summary (SUMMARIZE): %s\n\n", sc.Test.FullName())
	if err := os.WriteFile(abs, []byte(header+strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		return err
	}
	sc.SummaryPath = filepath.ToSlash(rel)
	sc.Summary = content
	if s.verbose || s.pauseFn != nil {
		fmt.Fprintf(os.Stdout, "--- SUMMARIZE output for %s ---\n%s\n--- end ---\n\n",
			sc.Test.FullName(), strings.TrimSpace(content))
	}
	if s.pauseFn != nil {
		s.pauseFn()
	}
	return nil
}

func (s *summarizeStage) buildAgent(test jenkins.FailedTest) (vnext.Agent, error) {
	name := "summarize-" + sanitize(test.FullName())
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: summarizeSystemPrompt,
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

const summarizeSystemPrompt = `You are a test-failure analyst. You will be given an investigation brief, a list of hypotheses, and the deep-inspection result for each hypothesis (if one completed).

Your output has three parts:

**Part 1 — Hypothesis summaries.** For EACH hypothesis, write a short paragraph:
- If a deep-inspection result exists: summarize what the inspector found (confirmed, refuted, or inconclusive) and the key evidence.
- If no result exists (the inspection failed or was not run): state that clearly and briefly restate what the hypothesis claimed.

**Part 2 — Alternative causes discovered.** A deep-inspection may have included an "Alternative Cause Discovered" section describing a root cause OUTSIDE its assigned hypothesis that it stumbled onto. If any inspection did, summarize each such alternative cause and its evidence here. If none did, write "None." and nothing more.

**Part 3 — Most likely root cause.** After the summaries, name the most likely root cause — but ONLY if it is well-supported by the evidence: either a hypothesis that was CONFIRMED by its deep-inspection, or an alternative cause that a deep-inspection reported with concrete file:line evidence. If nothing is well-supported (all hypotheses were REFUTED, INCONCLUSIVE, or had no result, and no evidenced alternative was found), write "No root cause was confirmed by the evidence" and do not guess.

Output ONLY Markdown with this structure (no preamble, no extra sections):

## Hypothesis Summaries

### Hypothesis 1: <title>
<paragraph>

### Hypothesis 2: <title>
<paragraph>

## Alternative Causes Discovered
<summary, or "None.">

## Most Likely Root Cause
<1–2 sentences>`

// buildSummarizePrompt assembles the first user message for SUMMARIZE.
func buildSummarizePrompt(test jenkins.FailedTest, brief string, hypotheses []Hypothesis, outcomes []DeepInspectOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summarize the investigation results for the failing test **%s**.\n\n", test.FullName())
	b.WriteString("## Investigation brief (from LOGPARSE)\n\n")
	b.WriteString(strings.TrimSpace(brief))
	b.WriteString("\n\n")
	b.WriteString(renderOutcomesForSummarize(hypotheses, outcomes))
	b.WriteString("\n\nFor each hypothesis, summarize what was found (or note that no result is available). Then state the most likely root cause.")
	return b.String()
}

func buildSummarizeRetryPrompt(test jenkins.FailedTest, brief string, hypotheses []Hypothesis, outcomes []DeepInspectOutcome, prevOutput, critique string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your previous summary for **%s** was reviewed and found insufficient.\n\n", test.FullName())
	b.WriteString("## What needs to be fixed\n\n")
	b.WriteString(strings.TrimSpace(critique))
	b.WriteString("\n\n## Your previous output (for reference)\n\n")
	b.WriteString(strings.TrimSpace(prevOutput))
	b.WriteString("\n\n## Investigation brief (from LOGPARSE)\n\n")
	b.WriteString(strings.TrimSpace(brief))
	b.WriteString("\n\n")
	b.WriteString(renderOutcomesForSummarize(hypotheses, outcomes))
	b.WriteString("\n\nProduce an improved summary that addresses every gap listed above.")
	return b.String()
}

// renderOutcomesForSummarize formats all hypothesis+DEEPINSPECT result pairs for
// inclusion in the SUMMARIZE prompt, clearly marking when no result is available.
func renderOutcomesForSummarize(hypotheses []Hypothesis, outcomes []DeepInspectOutcome) string {
	var b strings.Builder
	b.WriteString("## Hypotheses and inspection results\n\n")
	for i, h := range hypotheses {
		fmt.Fprintf(&b, "### Hypothesis %d: %s\n\n%s\n\n", h.Index, h.Title, h.Description)
		if i >= len(outcomes) {
			b.WriteString("**Inspection result: NOT AVAILABLE** — no inspection was run for this hypothesis.\n\n")
			continue
		}
		o := outcomes[i]
		if o.Failed {
			fmt.Fprintf(&b, "**Inspection result: NOT AVAILABLE** — the inspection could not complete: %s\n\n", o.FailReason)
		} else if strings.TrimSpace(o.Content) == "" {
			b.WriteString("**Inspection result: NOT AVAILABLE** — the inspection produced no content.\n\n")
		} else {
			status := "FEEDBACK APPROVED"
			if !o.FeedbackApproved {
				status = "feedback not run"
			}
			fmt.Fprintf(&b, "**Inspection result: %s**\n\n%s\n\n", status, strings.TrimSpace(o.Content))
		}
	}
	return b.String()
}
