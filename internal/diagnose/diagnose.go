// Package diagnose implements the DEEPINSPECT stage: it runs a single failing
// test through our own tool-using investigation loop (internal/inspect) that
// reads workspace SOURCE files and determines the root cause.
//
// Each call to Diagnose runs one attempt for one hypothesis. The feedback/retry
// loop is managed externally by the pipeline (deepInspectAllStage + feedback
// stage), so this package is a pure "run one investigation, return the result"
// layer.
//
// The agent is NOT given the raw Jenkins log. It works from the investigation
// brief produced by LOGPARSE and the specific hypothesis from HYPOTHESIZE. The
// raw-log tools are hard-disabled for the duration of the run.
package diagnose

import (
	"context"
	"fmt"
	"os"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/failmode"
	"github.com/gilbertr/testdiag/internal/inspect"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/mapping"
	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// DiagnoseInput carries everything a single DEEPINSPECT attempt needs.
type DiagnoseInput struct {
	Test            jenkins.FailedTest
	Brief           string // LOGPARSE handoff (not the raw log)
	Hypothesis      string // the full hypothesis text to investigate
	HypothesisIndex int    // 1-based index
	Plan            string // PLAN output: annotated file list (may be empty)
	PrevResult      string // empty on first attempt; prior draft for retry
	Critique        string // empty on first attempt; feedback for retry
}

// Result is the outcome of one DEEPINSPECT attempt.
type Result struct {
	Content       string   // the agent's Markdown analysis
	ToolsCalled   []string // tools the agent invoked (for reporting)
	KnowledgeJSON []byte   // JSON dump of the accumulated fact tree (debug artifact)
}

// Diagnoser runs the DEEPINSPECT stage against a fixed workspace using the LLM
// assigned to that stage.
type Diagnoser struct {
	ws                *workspace.Workspace
	llm               config.LLMSpec
	mode              failmode.Mode // flaky (default) vs always-fails
	background        string        // contents of TEST_AGENT.md
	memory            string        // contents of .testdiag/memory.md (may be empty)
	maxToolIterations int
	maxChars          int
	mapper            string              // path to test→source mapper executable; may be empty
	interrupt         inspect.Interrupter // operator-interrupt console; may be nil
	drainFn           func()              // called at the start of each Diagnose(); may be nil
}

// New creates a Diagnoser. llm is the LLM assigned to the DEEPINSPECT stage;
// mode selects flaky vs always-fails framing;
// background is the TEST_AGENT.md content (may be "");
// memory is the contents of .testdiag/memory.md (may be "");
// maxToolIterations caps the tool-calling loop per attempt;
// maxChars caps the accumulated knowledge rendered into context each turn;
// mapper is the optional path to the test→source mapping executable;
// interrupt, if non-nil, lets an operator inject guidance mid-run;
// drainFn, if non-nil, is called before each attempt to discard any queued
// operator messages that arrived between hypothesis runs.
func New(ws *workspace.Workspace, llm config.LLMSpec, mode failmode.Mode, background, memory string, maxToolIterations, maxChars int, mapper string, interrupt inspect.Interrupter, drainFn func()) *Diagnoser {
	return &Diagnoser{
		ws: ws, llm: llm, mode: mode, background: background, memory: memory,
		maxToolIterations: maxToolIterations, maxChars: maxChars, mapper: mapper,
		interrupt: interrupt, drainFn: drainFn,
	}
}

// Diagnose runs one DEEPINSPECT attempt for one hypothesis. When input.PrevResult
// and input.Critique are non-empty this is a feedback-triggered retry: the
// previous draft and the feedback are included in the task so the agent knows
// exactly what to improve. Each call accumulates its own fresh knowledge tree,
// so prior runs have no effect on this one.
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

	// Hard-block the raw failure log: DEEPINSPECT works only from the brief.
	tools.SetLogToolsEnabled(false)
	defer tools.SetLogToolsEnabled(true)

	// DEEPINSPECT never needs the raw log or the notebook (the knowledge tree is
	// its working memory now), but it keeps run_script for verification.
	exclude := append(append([]string{}, tools.LogToolNames...), "notebook")
	engine := inspect.NewEngine(d.llm, inspect.Options{
		MaxIterations: d.maxToolIterations,
		MaxChars:      d.maxChars,
		Schemas:       tools.SchemasExcluding(exclude...),
		Interrupt:     d.interrupt,
	})

	tools.ResetLoopGuard()
	tools.ResetSearchCache()
	tools.ResetFindFilesCache()

	r, err := engine.Run(ctx, inspect.RunInput{
		System: buildSystemPrompt(d.mode, input.Brief, input.Hypothesis, input.Plan, m.SourceFile, d.maxToolIterations),
		Task:   buildUserPrompt(input, d.background, d.memory),
	})
	if err != nil {
		return Result{}, fmt.Errorf("agent run for %s: %w", input.Test.FullName(), err)
	}

	out := Result{Content: r.Content, ToolsCalled: r.ToolsCalled}
	if r.Store != nil {
		out.KnowledgeJSON, _ = r.Store.JSON()
	}
	return out, nil
}
