package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gilramir/testdiag/internal/config"
	"github.com/gilramir/testdiag/internal/inspect"
	"github.com/gilramir/testdiag/internal/jenkins"
	"github.com/gilramir/testdiag/internal/workspace"
)

// setGoalsAllStage runs one SETGOALS+FEEDBACK pass per hypothesis. It sits
// between PLANINSPECTION and DEEPINSPECT: for each hypothesis it reads the
// PLANINSPECTION file list and the hypothesis itself and writes a concrete,
// step-by-step list of inspection goals that drives DEEPINSPECT — which files
// to read, what to look for, and what to do whether or not it is found. The
// stage is tool-less (the planner already surveyed the workspace). A hypothesis
// whose goals fail is recorded as a failed outcome and does NOT stop the
// pipeline — DEEPINSPECT will work from the plan (or the brief) alone.
type setGoalsAllStage struct {
	ws           *workspace.Workspace
	llm          config.LLMSpec
	feedback     *feedbackChecker // nil when SETGOALS feedback is disabled
	maxFeedbacks int
	verbose      bool
	pauseFn      func() // non-nil when -p is set; called after each handoff print
}

func newSetGoalsAllStage(ws *workspace.Workspace, llm config.LLMSpec, fb *feedbackChecker, maxFeedbacks int, verbose bool, pauseFn func()) *setGoalsAllStage {
	return &setGoalsAllStage{ws: ws, llm: llm, feedback: fb, maxFeedbacks: maxFeedbacks, verbose: verbose, pauseFn: pauseFn}
}

func (s *setGoalsAllStage) Name() State { return StateSetGoals }

func (s *setGoalsAllStage) Run(ctx context.Context, sc *Context) error {
	sc.SetGoals = make([]SetGoalsOutcome, 0, len(sc.Hypotheses))
	for i, h := range sc.Hypotheses {
		if ctx.Err() != nil {
			sc.SetGoals = append(sc.SetGoals, SetGoalsOutcome{
				Hypothesis: h, Failed: true, FailReason: "context cancelled",
			})
			continue
		}
		// Pass the PLANINSPECTION output for this hypothesis, if available and successful.
		var planContent string
		if i < len(sc.Plans) && !sc.Plans[i].Failed {
			planContent = sc.Plans[i].Content
		}
		sc.SetGoals = append(sc.SetGoals, s.runOne(ctx, sc, h, planContent))
	}
	return nil
}

// runOne runs the SETGOALS+FEEDBACK loop for one hypothesis. It never returns an
// error; failures are captured in the returned outcome.
func (s *setGoalsAllStage) runOne(ctx context.Context, sc *Context, h Hypothesis, planContent string) SetGoalsOutcome {
	out := SetGoalsOutcome{Hypothesis: h}

	if s.verbose || s.pauseFn != nil {
		fmt.Fprintf(os.Stdout, "--- handoff to SETGOALS h%d/%d for %s ---\n%s\n--- end ---\n\n",
			h.Index, len(sc.Hypotheses), sc.Test.FullName(), h.Text())
	}
	if s.pauseFn != nil {
		s.pauseFn()
	}

	var (
		prevOutput string
		critique   string
	)
	for feedbacks := 0; ; {
		stageBanner(s.verbose, fmt.Sprintf("%s h%d", string(s.Name()), h.Index), feedbacks+1)
		var prompt string
		if critique == "" {
			prompt = buildSetGoalsPrompt(sc.Test, sc.Brief, h, planContent)
		} else {
			prompt = buildSetGoalsRetryPrompt(sc.Test, sc.Brief, h, planContent, prevOutput, critique)
		}
		raw, err := inspect.Complete(ctx, s.llm, setGoalsSystemPrompt, prompt)
		if err != nil {
			out.Failed = true
			out.FailReason = err.Error()
			if s.verbose {
				fmt.Fprintf(os.Stdout, "  SETGOALS h%d error: %v\n", h.Index, err)
			}
			return out
		}
		content := strings.TrimSpace(raw)
		if content == "" {
			out.Failed = true
			out.FailReason = "SETGOALS returned empty output"
			return out
		}
		out.Content = content

		if s.feedback == nil {
			out.FeedbackApproved = true
			return s.save(sc, h, out)
		}

		ok, newCritique, err := s.feedback.Check(ctx, sc.Test, content, "")
		if err != nil {
			out.Failed = true
			out.FailReason = fmt.Sprintf("feedback error: %v", err)
			if s.verbose {
				fmt.Fprintf(os.Stdout, "  SETGOALS h%d FEEDBACK error: %v\n", h.Index, err)
			}
			return out
		}
		if s.verbose {
			if ok {
				fmt.Fprintf(os.Stdout, "  SETGOALS h%d FEEDBACK: APPROVED\n", h.Index)
			} else {
				fmt.Fprintf(os.Stdout, "  SETGOALS h%d FEEDBACK: NEEDS REVISION: %s\n", h.Index, newCritique)
			}
		}
		if ok {
			out.FeedbackApproved = true
			return s.save(sc, h, out)
		}
		feedbacks++
		if feedbacks >= s.maxFeedbacks {
			out.Failed = true
			out.FailReason = fmt.Sprintf("did not meet goals after %d feedback(s): %s", feedbacks, newCritique)
			return out
		}
		prevOutput = content
		critique = newCritique
	}
}

func (s *setGoalsAllStage) save(sc *Context, h Hypothesis, out SetGoalsOutcome) SetGoalsOutcome {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if s.verbose {
			fmt.Fprintf(os.Stderr, "  SETGOALS h%d: could not create handoff dir: %v\n", h.Index, err)
		}
		return out
	}
	base := fmt.Sprintf("%s.h%d.setgoals.md", sanitize(sc.Test.FullName()), h.Index)
	abs := filepath.Join(dir, base)
	header := fmt.Sprintf("# Inspection Goals (SETGOALS) h%d: %s\n\n", h.Index, sc.Test.FullName())
	if err := os.WriteFile(abs, []byte(header+strings.TrimSpace(out.Content)+"\n"), 0o644); err != nil {
		if s.verbose {
			fmt.Fprintf(os.Stderr, "  SETGOALS h%d: could not write handoff file: %v\n", h.Index, err)
		}
	}
	if s.verbose || s.pauseFn != nil {
		fmt.Fprintf(os.Stdout, "--- SETGOALS h%d output for %s ---\n%s\n--- end ---\n\n",
			h.Index, sc.Test.FullName(), strings.TrimSpace(out.Content))
	}
	if s.pauseFn != nil {
		s.pauseFn()
	}
	return out
}

const setGoalsSystemPrompt = `You are an investigation lead. A planning stage (PLANINSPECTION) has already surveyed the workspace for ONE specific hypothesis about a failing test and produced a prioritized, annotated list of files. Your job is to turn that survey into a concrete, step-by-step list of INSPECTION GOALS that will drive the next stage, DEEPINSPECT — a tool-using agent that reads source files to confirm or refute the hypothesis.

You do NOT have tools and you do NOT investigate the code yourself. You read the hypothesis, the investigation brief, and the inspection file list, and you write an ordered plan of goals for DEEPINSPECT to execute.

Each goal must be a numbered step that tells DEEPINSPECT:
- WHICH file (and, where the plan names them, which symbol/function/line region) to examine — use the concrete paths from the inspection plan.
- WHAT it is looking for there — the specific code, condition, value, call, or relationship that bears on the hypothesis.
- WHAT IT MEANS IF FOUND — how that finding moves the hypothesis toward CONFIRMED or REFUTED, and which step to do next.
- WHAT IT MEANS IF NOT FOUND — the fallback: which file or symbol to check instead, or whether the absence is itself evidence.

Order the steps so the most decisive checks come first. Cover BOTH sides of the boundary the hypothesis turns on (the code that would make it true AND the code that would make it false). Do not invent file paths — use only paths that appear in the inspection plan (or, if the plan is missing, name the symbols to locate first).

Output ONLY Markdown with this exact structure (no preamble, no extra sections):

## Inspection Goals
1. **` + "`<file path>`" + `** — <what to look for>. If found: <implication + next step>. If not found: <fallback>.
2. **` + "`<file path>`" + `** — <what to look for>. If found: <implication + next step>. If not found: <fallback>.

## Verdict Criteria
A short note stating what evidence would justify CONFIRMED, what would justify REFUTED, and what would leave the result INCONCLUSIVE for this hypothesis.`

// buildSetGoalsPrompt assembles the first user message for one SETGOALS attempt.
func buildSetGoalsPrompt(test jenkins.FailedTest, brief string, h Hypothesis, plan string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write the step-by-step inspection goals that will drive DEEPINSPECT for the failing test **%s**.\n\n", test.FullName())
	writeSetGoalsContext(&b, brief, h, plan)
	b.WriteString("\nProduce the ordered inspection goals and verdict criteria now.")
	return b.String()
}

// buildSetGoalsRetryPrompt assembles the user message for a feedback-triggered
// SETGOALS retry.
func buildSetGoalsRetryPrompt(test jenkins.FailedTest, brief string, h Hypothesis, plan, prevOutput, critique string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your previous inspection goals for **%s** were reviewed and found insufficient.\n\n", test.FullName())
	b.WriteString("## What needs to be fixed\n\n")
	b.WriteString(strings.TrimSpace(critique))
	b.WriteString("\n\n## Your previous output (for reference)\n\n")
	b.WriteString(strings.TrimSpace(prevOutput))
	b.WriteString("\n\n")
	writeSetGoalsContext(&b, brief, h, plan)
	b.WriteString("\nProduce improved inspection goals that address every gap listed above.")
	return b.String()
}

// writeSetGoalsContext appends the shared context (hypothesis, brief, plan) used
// by both the first-attempt and retry SETGOALS prompts.
func writeSetGoalsContext(b *strings.Builder, brief string, h Hypothesis, plan string) {
	fmt.Fprintf(b, "## Hypothesis to plan around\n\n%s\n\n", h.Text())
	if strings.TrimSpace(brief) != "" {
		b.WriteString("## Investigation brief (from LOGPARSE)\n\n")
		b.WriteString(strings.TrimSpace(brief))
		b.WriteString("\n\n")
	}
	b.WriteString("## Inspection plan (from PLANINSPECTION)\n\n")
	if strings.TrimSpace(plan) != "" {
		b.WriteString(strings.TrimSpace(plan))
	} else {
		b.WriteString("_No inspection plan is available for this hypothesis._ Base your goals on the hypothesis and brief, naming the key symbols DEEPINSPECT should locate first.")
	}
	b.WriteString("\n\n")
}
