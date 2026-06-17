package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gilramir/testdiag/internal/diagnose"
	"github.com/gilramir/testdiag/internal/tools"
	"github.com/gilramir/testdiag/internal/workspace"
)

// deepInspectAllStage runs one DEEPINSPECT+FEEDBACK pass per hypothesis from
// the HYPOTHESIZE stage. A hypothesis that fails (agent error or feedback
// exhausted) is recorded as a failed outcome but does NOT stop the pipeline —
// the SUMMARIZE stage will work with whatever results are available.
type deepInspectAllStage struct {
	d            *diagnose.Diagnoser
	ws           *workspace.Workspace
	feedback     *feedbackChecker // nil when DEEPINSPECT feedback is disabled
	maxFeedbacks int
	verbose      bool
	pauseFn      func() // non-nil when -p is set; called after each handoff print
}

func newDeepInspectAllStage(d *diagnose.Diagnoser, ws *workspace.Workspace, fb *feedbackChecker, maxFeedbacks int, verbose bool, pauseFn func()) *deepInspectAllStage {
	return &deepInspectAllStage{d: d, ws: ws, feedback: fb, maxFeedbacks: maxFeedbacks, verbose: verbose, pauseFn: pauseFn}
}

func (s *deepInspectAllStage) Name() State { return StateDeepInspect }

func (s *deepInspectAllStage) Run(ctx context.Context, sc *Context) error {
	sc.DeepInspects = make([]DeepInspectOutcome, 0, len(sc.Hypotheses))
	for i, h := range sc.Hypotheses {
		if ctx.Err() != nil {
			sc.DeepInspects = append(sc.DeepInspects, DeepInspectOutcome{
				Hypothesis: h, Failed: true, FailReason: "context cancelled",
			})
			continue
		}
		// Pass the PLANINSPECTION output for this hypothesis, if available and successful.
		var planContent string
		if i < len(sc.Plans) && !sc.Plans[i].Failed {
			planContent = sc.Plans[i].Content
		}
		// Pass the SETGOALS output for this hypothesis, if available and successful.
		var goalsContent string
		if i < len(sc.SetGoals) && !sc.SetGoals[i].Failed {
			goalsContent = sc.SetGoals[i].Content
		}
		tools.ResetToolLog()
		out := s.runOne(ctx, sc, h, planContent, goalsContent)
		s.writeToolLog(sc, h, tools.CollectToolLog())
		sc.DeepInspects = append(sc.DeepInspects, out)
	}
	return nil
}

// runOne runs the DEEPINSPECT+FEEDBACK loop for one hypothesis. It never
// returns an error; failures are captured in the returned outcome.
func (s *deepInspectAllStage) runOne(ctx context.Context, sc *Context, h Hypothesis, planContent, goalsContent string) DeepInspectOutcome {
	out := DeepInspectOutcome{Hypothesis: h}

	if s.verbose || s.pauseFn != nil {
		fmt.Fprintf(os.Stdout, "--- handoff to DEEPINSPECT h%d/%d for %s ---\n%s\n--- end ---\n\n",
			h.Index, len(sc.Hypotheses), sc.Test.FullName(), h.Text())
	}
	if s.pauseFn != nil {
		s.pauseFn()
	}

	var (
		prevResult string
		critique   string
	)
	for feedbacks := 0; ; {
		stageBanner(s.verbose, fmt.Sprintf("%s h%d", string(s.Name()), h.Index), feedbacks+1)
		res, err := s.d.Diagnose(ctx, diagnose.DiagnoseInput{
			Test:            sc.Test,
			Brief:           sc.Brief,
			LogPath:         sc.LogPath,
			Hypothesis:      h.Text(),
			HypothesisIndex: h.Index,
			Plan:            planContent,
			Goals:           goalsContent,
			PrevResult:      prevResult,
			Critique:        critique,
		})
		if err != nil {
			out.Failed = true
			out.FailReason = err.Error()
			if s.verbose {
				fmt.Fprintf(os.Stdout, "  DEEPINSPECT h%d error: %v\n", h.Index, err)
			}
			return out
		}
		out.Content = res.Content
		out.ToolsCalled = res.ToolsCalled
		s.writeKnowledge(sc, h, res.KnowledgeJSON)

		if s.feedback == nil {
			out.FeedbackApproved = true
			return s.save(sc, h, out)
		}

		ok, newCritique, err := s.feedback.Check(ctx, sc.Test, res.Content, peekToolLog())
		if err != nil {
			// A feedback error on a hypothesis is non-fatal: mark as failed.
			out.Failed = true
			out.FailReason = fmt.Sprintf("feedback error: %v", err)
			if s.verbose {
				fmt.Fprintf(os.Stdout, "  DEEPINSPECT h%d FEEDBACK error: %v\n", h.Index, err)
			}
			return s.save(sc, h, out)
		}
		if s.verbose {
			if ok {
				fmt.Fprintf(os.Stdout, "  DEEPINSPECT h%d FEEDBACK: APPROVED\n", h.Index)
			} else {
				fmt.Fprintf(os.Stdout, "  DEEPINSPECT h%d FEEDBACK: NEEDS REVISION: %s\n", h.Index, newCritique)
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
			return s.save(sc, h, out)
		}
		prevResult = res.Content
		critique = newCritique
	}
}

func (s *deepInspectAllStage) writeToolLog(sc *Context, h Hypothesis, calls []tools.ToolCall) {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	base := fmt.Sprintf("%s.h%d.deepinspect.tools.md", sanitize(sc.Test.FullName()), h.Index)
	header := fmt.Sprintf("# Tool Log (DEEPINSPECT) h%d: %s\n\n", h.Index, sc.Test.FullName())
	_ = os.WriteFile(filepath.Join(dir, base), []byte(header+tools.FormatToolLog(calls)), 0o644)
}

// writeKnowledge dumps the accumulated fact tree (JSON) for one hypothesis as a
// debugging artifact next to the tool log.
func (s *deepInspectAllStage) writeKnowledge(sc *Context, h Hypothesis, data []byte) {
	if len(data) == 0 {
		return
	}
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	base := fmt.Sprintf("%s.h%d.deepinspect.knowledge.json", sanitize(sc.Test.FullName()), h.Index)
	_ = os.WriteFile(filepath.Join(dir, base), data, 0o644)
}

// save writes the DEEPINSPECT result to a handoff file so the distillation
// stage can read it later. It always returns out unchanged so callers can
// write `return s.save(sc, h, out)` without a separate variable.
func (s *deepInspectAllStage) save(sc *Context, h Hypothesis, out DeepInspectOutcome) DeepInspectOutcome {
	if strings.TrimSpace(out.Content) == "" {
		return out
	}
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if s.verbose {
			fmt.Fprintf(os.Stderr, "  DEEPINSPECT h%d: could not create handoff dir: %v\n", h.Index, err)
		}
		return out
	}
	base := fmt.Sprintf("%s.h%d.deepinspect.md", sanitize(sc.Test.FullName()), h.Index)
	abs := filepath.Join(s.ws.Root(), handoffDir, base)
	header := fmt.Sprintf("# Deep Inspection (DEEPINSPECT) h%d: %s\n\n", h.Index, sc.Test.FullName())
	if err := os.WriteFile(abs, []byte(header+strings.TrimSpace(out.Content)+"\n"), 0o644); err != nil {
		if s.verbose {
			fmt.Fprintf(os.Stderr, "  DEEPINSPECT h%d: could not write handoff file: %v\n", h.Index, err)
		}
	} else if s.verbose || s.pauseFn != nil {
		fmt.Fprintf(os.Stdout, "--- DEEPINSPECT h%d output for %s ---\n%s\n--- end ---\n\n",
			h.Index, sc.Test.FullName(), strings.TrimSpace(out.Content))
	}
	if s.pauseFn != nil {
		s.pauseFn()
	}
	return out
}
