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

// combineStage reads all hypotheses and DEEPINSPECT results and asks an LLM
// to select the best-supported root cause. A FEEDBACK gate checks the output;
// if rejected, COMBINE retries with the critique, up to maxFeedbacks times.
type combineStage struct {
	ws           *workspace.Workspace
	llm          config.LLMSpec
	feedback     *feedbackChecker
	maxFeedbacks int
	verbose      bool
}

func newCombineStage(ws *workspace.Workspace, llm config.LLMSpec, fb *feedbackChecker, maxFeedbacks int, verbose bool) *combineStage {
	return &combineStage{ws: ws, llm: llm, feedback: fb, maxFeedbacks: maxFeedbacks, verbose: verbose}
}

func (s *combineStage) Name() State { return StateCombine }

func (s *combineStage) Run(ctx context.Context, sc *Context) error {
	var (
		prevOutput string
		critique   string
	)
	for feedbacks := 0; ; {
		agent, err := s.buildAgent(sc.Test)
		if err != nil {
			return fmt.Errorf("building agent: %w", err)
		}
		var prompt string
		if critique == "" {
			prompt = buildCombinePrompt(sc.Test, sc.Brief, sc.Hypotheses, sc.DeepInspects)
		} else {
			prompt = buildCombineRetryPrompt(sc.Test, sc.Brief, sc.Hypotheses, sc.DeepInspects, prevOutput, critique)
		}
		r, err := agent.Run(ctx, prompt)
		if err != nil {
			return fmt.Errorf("agent run: %w", err)
		}
		content := strings.TrimSpace(r.Content)
		if content == "" {
			return fmt.Errorf("COMBINE agent returned empty output for %s", sc.Test.FullName())
		}

		if s.feedback == nil {
			return s.save(sc, content)
		}

		ok, newCritique, err := s.feedback.Check(ctx, sc.Test, content)
		if err != nil {
			return fmt.Errorf("feedback: %w", err)
		}
		if s.verbose {
			if ok {
				fmt.Fprintf(os.Stdout, "  COMBINE FEEDBACK: APPROVED\n")
			} else {
				fmt.Fprintf(os.Stdout, "  COMBINE FEEDBACK: NEEDS REVISION: %s\n", newCritique)
			}
		}
		if ok {
			return s.save(sc, content)
		}
		feedbacks++
		if feedbacks >= s.maxFeedbacks {
			return fmt.Errorf("%s: COMBINE did not meet goals after %d feedback(s): %s",
				sc.Test.FullName(), feedbacks, newCritique)
		}
		prevOutput = content
		critique = newCritique
	}
}

func (s *combineStage) save(sc *Context, content string) error {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rel := filepath.Join(handoffDir, sanitize(sc.Test.FullName())+".combine.md")
	abs := filepath.Join(s.ws.Root(), rel)
	header := fmt.Sprintf("# Root cause (COMBINE): %s\n\n", sc.Test.FullName())
	if err := os.WriteFile(abs, []byte(header+strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		return err
	}
	sc.CombinePath = filepath.ToSlash(rel)
	sc.Combined = content
	if s.verbose {
		fmt.Fprintf(os.Stdout, "--- COMBINE output for %s ---\n%s\n--- end ---\n\n",
			sc.Test.FullName(), strings.TrimSpace(content))
	}
	return nil
}

func (s *combineStage) buildAgent(test jenkins.FailedTest) (vnext.Agent, error) {
	name := "combine-" + sanitize(test.FullName())
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: combineSystemPrompt,
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

const combineSystemPrompt = `You are a root-cause synthesis analyst. You will be given:
- An investigation brief from a log-analysis stage
- A list of hypotheses about what could have caused the failure
- The result of a deep code inspection for each hypothesis (some may be failed/inconclusive)

Your job is to select the best-supported hypothesis and produce a final root-cause report. If multiple hypotheses are partially supported, synthesize them into the most coherent explanation.

Output ONLY Markdown with exactly these sections:
## Summary
One sentence: what went wrong and why it is intermittent.
## Selected Hypothesis
Which hypothesis was best supported and why (briefly note the others and why they were ranked lower or ruled out).
## Root Cause
The specific nondeterministic condition: the race, ordering, timing, resource, or environment issue.
## Evidence
File paths, line numbers, and quoted code from the DEEPINSPECT results that support the conclusion.
## Why It's Flaky
What must differ between a passing and a failing run.
## Suggested Fix
Concrete change(s) that would eliminate the nondeterminism.
## Confidence
High / Medium / Low and why.`

// buildCombinePrompt assembles the first user message for COMBINE.
func buildCombinePrompt(test jenkins.FailedTest, brief string, hypotheses []Hypothesis, outcomes []DeepInspectOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Synthesize the root cause for the failing test **%s**.\n\n", test.FullName())
	b.WriteString("## Investigation brief (from LOGPARSE)\n\n")
	b.WriteString(strings.TrimSpace(brief))
	b.WriteString("\n\n")
	b.WriteString(renderOutcomesForCombine(hypotheses, outcomes))
	b.WriteString("\n\nSelect the best-supported hypothesis and write the final root-cause report in the required Markdown format.")
	return b.String()
}

func buildCombineRetryPrompt(test jenkins.FailedTest, brief string, hypotheses []Hypothesis, outcomes []DeepInspectOutcome, prevOutput, critique string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your previous combined analysis for **%s** was reviewed and found insufficient.\n\n", test.FullName())
	b.WriteString("## What needs to be fixed\n\n")
	b.WriteString(strings.TrimSpace(critique))
	b.WriteString("\n\n## Your previous output (for reference)\n\n")
	b.WriteString(strings.TrimSpace(prevOutput))
	b.WriteString("\n\n## Investigation brief (from LOGPARSE)\n\n")
	b.WriteString(strings.TrimSpace(brief))
	b.WriteString("\n\n")
	b.WriteString(renderOutcomesForCombine(hypotheses, outcomes))
	b.WriteString("\n\nProduce an improved analysis that addresses every gap listed above.")
	return b.String()
}

// renderOutcomesForCombine formats all hypothesis+DEEPINSPECT result pairs for
// inclusion in the COMBINE prompt.
func renderOutcomesForCombine(hypotheses []Hypothesis, outcomes []DeepInspectOutcome) string {
	var b strings.Builder
	b.WriteString("## Hypotheses and DEEPINSPECT results\n\n")
	for i, h := range hypotheses {
		fmt.Fprintf(&b, "### Hypothesis %d: %s\n\n%s\n\n", h.Index, h.Title, h.Description)
		if i < len(outcomes) {
			o := outcomes[i]
			if o.Failed {
				fmt.Fprintf(&b, "**DEEPINSPECT result: FAILED** — %s\n\n", o.FailReason)
			} else {
				status := "FEEDBACK APPROVED"
				if !o.FeedbackApproved {
					status = "feedback not run"
				}
				fmt.Fprintf(&b, "**DEEPINSPECT result: %s**\n\n%s\n\n", status, strings.TrimSpace(o.Content))
			}
		}
	}
	return b.String()
}
