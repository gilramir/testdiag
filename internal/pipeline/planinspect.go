package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gilbertr/testdiag/internal/planner"
	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// peekToolLog returns the current tool call log formatted as Markdown,
// without draining it so the outer CollectToolLog still captures everything.
func peekToolLog() string {
	return tools.FormatToolLog(tools.PeekToolLog())
}

// planInspectAllStage runs one PLANINSPECTION+FEEDBACK pass per hypothesis
// from HYPOTHESIZE. A hypothesis whose plan fails is recorded as a failed
// outcome and does NOT stop the pipeline — DEEPINSPECT will work from the
// brief alone for that hypothesis.
type planInspectAllStage struct {
	p            *planner.Planner
	ws           *workspace.Workspace
	archDocPath  string
	feedback     *feedbackChecker
	maxFeedbacks int
	resetCounter func() // resets the proxy's per-run request counter; may be nil
	verbose      bool
	pauseFn      func() // non-nil when -p is set; called after each handoff print
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
		stageBanner(s.verbose, fmt.Sprintf("%s h%d", string(s.Name()), h.Index), feedbacks+1)
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
		s.writeKnowledge(sc, h, res.KnowledgeJSON)

		// Deterministic gate: every file path the plan lists must actually
		// exist in the workspace. The LLM cannot be trusted to verify this, so
		// Go checks it directly and forces a revision (consuming a feedback
		// turn) when the plan names files that are not there.
		if missing := missingPlanFiles(s.ws, res.Content); len(missing) > 0 {
			critiqueText := buildMissingFilesCritique(missing)
			if s.verbose {
				fmt.Fprintf(os.Stdout, "  PLANINSPECTION h%d: %d listed file(s) do not exist: %s\n",
					h.Index, len(missing), strings.Join(missing, ", "))
			}
			feedbacks++
			if feedbacks >= s.maxFeedbacks {
				out.Failed = true
				out.FailReason = fmt.Sprintf("did not meet goals after %d feedback(s): %s", feedbacks, critiqueText)
				return out
			}
			prevResult = res.Content
			critique = critiqueText
			continue
		}

		if s.feedback == nil {
			out.FeedbackApproved = true
			return s.save(sc, h, out)
		}

		ok, newCritique, err := s.feedback.Check(ctx, sc.Test, res.Content, peekToolLog())
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

// writeKnowledge dumps the accumulated fact tree (JSON) for one hypothesis as a
// debugging artifact next to the tool log.
func (s *planInspectAllStage) writeKnowledge(sc *Context, h Hypothesis, data []byte) {
	if len(data) == 0 {
		return
	}
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	base := fmt.Sprintf("%s.h%d.planinspect.knowledge.json", sanitize(sc.Test.FullName()), h.Index)
	_ = os.WriteFile(filepath.Join(dir, base), data, 0o644)
}

// listItemRe matches a Markdown list-item marker at the start of a line
// (-, *, + or "1."), allowing leading indentation for nested entries.
var listItemRe = regexp.MustCompile(`^\s*(?:[-*+]|\d+\.)\s+`)

// firstBacktickRe captures the first backtick-delimited token on a line.
var firstBacktickRe = regexp.MustCompile("`([^`]+)`")

// extractPlanFiles pulls the candidate workspace-relative file path out of each
// list item in a PLANINSPECTION plan. The planner prompt mandates that every
// entry begin with the file path in backticks, so the FIRST backtick token on a
// list line is the path; later backtick tokens (e.g. a `grep: pattern`
// annotation) are ignored. Tokens containing whitespace are skipped — real
// paths have none, and prose wrapped in backticks should not be treated as a
// file to verify.
func extractPlanFiles(content string) []string {
	var paths []string
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		if !listItemRe.MatchString(line) {
			continue
		}
		m := firstBacktickRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		p := strings.TrimSpace(m[1])
		if p == "" || strings.ContainsAny(p, " \t") || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	return paths
}

// missingPlanFiles returns the plan's listed paths that do not exist in the
// workspace, in listing order.
func missingPlanFiles(ws *workspace.Workspace, content string) []string {
	var missing []string
	for _, p := range extractPlanFiles(content) {
		if !planFileExists(ws, p) {
			missing = append(missing, p)
		}
	}
	return missing
}

// planFileExists reports whether a plan entry resolves to something on disk.
// Glob-looking entries are matched with filepath.Glob (at least one hit);
// everything else is resolved through the workspace jail and stat'd.
func planFileExists(ws *workspace.Workspace, p string) bool {
	if strings.ContainsAny(p, "*?[") {
		rel := strings.TrimPrefix(filepath.Clean(p), string(filepath.Separator))
		matches, err := filepath.Glob(filepath.Join(ws.Root(), rel))
		return err == nil && len(matches) > 0
	}
	abs, err := ws.Resolve(p)
	if err != nil {
		return false
	}
	_, err = os.Stat(abs)
	return err == nil
}

// buildMissingFilesCritique renders the revision critique sent back to the
// planner when it lists files that do not exist.
func buildMissingFilesCritique(missing []string) string {
	var b strings.Builder
	b.WriteString("The following listed file path(s) do not exist in the workspace:\n")
	for _, p := range missing {
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	b.WriteString("\nEvery file in the plan MUST be a concrete workspace-relative path that exists. " +
		"Remove these entries or replace them with real paths, and confirm each one with the " +
		"file_exists, find_files, or function_lookup tool before listing it.")
	return b.String()
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
