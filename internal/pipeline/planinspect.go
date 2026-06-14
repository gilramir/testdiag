package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gilbertr/testdiag/internal/planner"
	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// planInspectAllStage runs one PLANINSPECTION+FEEDBACK pass per hypothesis
// from HYPOTHESIZE. A hypothesis whose plan fails is recorded as a failed
// outcome and does NOT stop the pipeline — DEEPINSPECT will work from the
// brief alone for that hypothesis.
type planInspectAllStage struct {
	p             *planner.Planner
	ws            *workspace.Workspace
	archDocPath   string
	feedback      *feedbackChecker
	maxFeedbacks  int
	resetCounter  func() // resets the proxy's per-run request counter; may be nil
	verbose       bool
	pauseFn       func() // non-nil when -p is set; called after each handoff print
}

func newPlanInspectAllStage(p *planner.Planner, ws *workspace.Workspace, archDocPath string, fb *feedbackChecker, maxFeedbacks int, resetCounter func(), verbose bool, pauseFn func()) *planInspectAllStage {
	return &planInspectAllStage{p: p, ws: ws, archDocPath: archDocPath, feedback: fb, maxFeedbacks: maxFeedbacks, resetCounter: resetCounter, verbose: verbose, pauseFn: pauseFn}
}

func (s *planInspectAllStage) Name() State { return StatePlanInspect }

func (s *planInspectAllStage) Run(ctx context.Context, sc *Context) error {
	archDoc := s.readArchDoc()
	sc.Plans = make([]PlanInspectOutcome, 0, len(sc.Hypotheses))
	for _, h := range sc.Hypotheses {
		if ctx.Err() != nil {
			sc.Plans = append(sc.Plans, PlanInspectOutcome{
				Hypothesis: h, Failed: true, FailReason: "context cancelled",
			})
			continue
		}
		tools.ResetToolLog()
		out := s.runOne(ctx, sc, h, archDoc)
		s.writeToolLog(sc, h, tools.CollectToolLog())
		sc.Plans = append(sc.Plans, out)
	}
	return nil
}

func (s *planInspectAllStage) runOne(ctx context.Context, sc *Context, h Hypothesis, archDoc string) PlanInspectOutcome {
	out := PlanInspectOutcome{Hypothesis: h}

	if s.resetCounter != nil {
		s.resetCounter()
	}

	if s.verbose || s.pauseFn != nil {
		fmt.Fprintf(os.Stdout, "--- handoff to PLANINSPECTION h%d/%d for %s ---\n%s\n--- end ---\n\n",
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
		res, err := s.p.Plan(ctx, planner.PlanInput{
			Test:            sc.Test,
			Brief:           sc.Brief,
			Hypothesis:      h.Text(),
			HypothesisIndex: h.Index,
			ArchDoc:         archDoc,
			PrevResult:      prevResult,
			Critique:        critique,
		})
		if err != nil {
			out.Failed = true
			out.FailReason = err.Error()
			if s.verbose {
				fmt.Fprintf(os.Stdout, "  PLANINSPECTION h%d error: %v\n", h.Index, err)
			}
			return out
		}
		out.Content = res.Content
		out.ToolsCalled = res.ToolsCalled

		if s.feedback == nil {
			out.FeedbackApproved = true
			return s.save(sc, h, out)
		}

		ok, newCritique, err := s.feedback.Check(ctx, sc.Test, res.Content)
		if err != nil {
			out.Failed = true
			out.FailReason = fmt.Sprintf("feedback error: %v", err)
			if s.verbose {
				fmt.Fprintf(os.Stdout, "  PLANINSPECTION h%d FEEDBACK error: %v\n", h.Index, err)
			}
			return out
		}
		if s.verbose {
			if ok {
				fmt.Fprintf(os.Stdout, "  PLANINSPECTION h%d FEEDBACK: APPROVED\n", h.Index)
			} else {
				fmt.Fprintf(os.Stdout, "  PLANINSPECTION h%d FEEDBACK: NEEDS REVISION: %s\n", h.Index, newCritique)
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
		prevResult = res.Content
		critique = newCritique
	}
}

func (s *planInspectAllStage) save(sc *Context, h Hypothesis, out PlanInspectOutcome) PlanInspectOutcome {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if s.verbose {
			fmt.Fprintf(os.Stderr, "  PLANINSPECTION h%d: could not create handoff dir: %v\n", h.Index, err)
		}
		return out
	}
	base := fmt.Sprintf("%s.h%d.planinspect.md", sanitize(sc.Test.FullName()), h.Index)
	rel := filepath.Join(handoffDir, base)
	abs := filepath.Join(s.ws.Root(), rel)
	header := fmt.Sprintf("# Inspection Plan (PLANINSPECTION) h%d: %s\n\n", h.Index, sc.Test.FullName())
	if err := os.WriteFile(abs, []byte(header+strings.TrimSpace(out.Content)+"\n"), 0o644); err != nil {
		if s.verbose {
			fmt.Fprintf(os.Stderr, "  PLANINSPECTION h%d: could not write handoff file: %v\n", h.Index, err)
		}
	}
	if s.verbose || s.pauseFn != nil {
		fmt.Fprintf(os.Stdout, "--- PLANINSPECTION h%d output for %s ---\n%s\n--- end ---\n\n",
			h.Index, sc.Test.FullName(), strings.TrimSpace(out.Content))
	}
	if s.pauseFn != nil {
		s.pauseFn()
	}
	return out
}

func (s *planInspectAllStage) writeToolLog(sc *Context, h Hypothesis, calls []tools.ToolCall) {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	base := fmt.Sprintf("%s.h%d.planinspect.tools.md", sanitize(sc.Test.FullName()), h.Index)
	header := fmt.Sprintf("# Tool Log (PLANINSPECTION) h%d: %s\n\n", h.Index, sc.Test.FullName())
	_ = os.WriteFile(filepath.Join(dir, base), []byte(header+tools.FormatToolLog(calls)), 0o644)
}

func (s *planInspectAllStage) readArchDoc() string {
	if s.archDocPath == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.ws.Root(), s.archDocPath))
	if err != nil {
		return ""
	}
	return string(data)
}
