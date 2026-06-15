// Package pipeline runs the per-test diagnosis as an explicit state machine:
//
//	DOWNLOAD        — materialize the failing test's log to .testdiag/logs/<test>.log
//	LOGPARSE        — one tool-less LLM pass that distils the log into an investigation
//	                 brief (.testdiag/handoff/<test>.logparse.md)
//	FEEDBACK        — checks the brief; rejects with a critique until it meets the goal
//	                 (up to StageConfig.LogParseMaxFeedbacks rejections)
//	HYPOTHESIZE     — reads the brief + the optional architecture document and produces
//	                 a ranked list of 1+ hypotheses (.testdiag/handoff/<test>.hypothesize.md);
//	                 0 hypotheses → the test is abandoned
//	FEEDBACK        — checks the hypothesis list (up to HypothesizeMaxFeedbacks)
//	PLANINSPECTION  — one tool-using agent per hypothesis; surveys the workspace and
//	                 produces an annotated file list for DEEPINSPECT to follow
//	                 (.testdiag/handoff/<test>.h<N>.planinspect.md); soft-fails per hypothesis
//	FEEDBACK        — checks each plan (up to PlanMaxFeedbacks)
//	DEEPINSPECT     — one agent run per hypothesis, jailed to workspace source tools;
//	                 receives both the hypothesis and the PLANINSPECTION output; each gets
//	                 its own FEEDBACK gate; a failed hypothesis is noted but does not stop the pipeline;
//	                 result saved to .testdiag/handoff/<test>.h<N>.deepinspect.md
//	SUMMARIZE       — summarizes each hypothesis (noting whether an inspection result
//	                 is available) and identifies the most likely root cause
//	                 (.testdiag/handoff/<test>.summarize.md)
//	FEEDBACK        — checks the summary (up to SummarizeMaxFeedbacks)
//	LESSONS         — tool-less meta-analysis: reads all handoffs + tool logs and
//	                 suggests improvements to the testdiag program itself
//	                 (.testdiag/handoff/<test>.lessons.md)
//
// After the pipeline the caller runs a MEMORIZE step (internal/distill) that
// extracts durable codebase facts from all handoff files and appends them to
// .testdiag/memory.md for use by future runs.
package pipeline

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gilramir/testdiag/internal/config"
	"github.com/gilramir/testdiag/internal/diagnose"
	"github.com/gilramir/testdiag/internal/failmode"
	"github.com/gilramir/testdiag/internal/inspect"
	"github.com/gilramir/testdiag/internal/jenkins"
	"github.com/gilramir/testdiag/internal/planner"
	"github.com/gilramir/testdiag/internal/workspace"
)

// State names the stages of the diagnosis state machine.
type State string

const (
	StateDownload    State = "DOWNLOAD"
	StateLogParse    State = "LOGPARSE"
	StateFeedback    State = "FEEDBACK"
	StateHypothsize  State = "HYPOTHESIZE"
	StatePlanInspect State = "PLANINSPECTION"
	StateDeepInspect State = "DEEPINSPECT"
	StateSummarize   State = "SUMMARIZE"
	StateLessons     State = "LESSONS"
	StateDone        State = "DONE"
)

// Hypothesis is one candidate explanation produced by HYPOTHESIZE.
type Hypothesis struct {
	Index       int // 1-based
	Title       string
	Description string
}

// Text returns the full Markdown text of the hypothesis (title + description),
// suitable for injecting into prompts.
func (h Hypothesis) Text() string {
	return fmt.Sprintf("### Hypothesis %d: %s\n\n%s", h.Index, h.Title, h.Description)
}

// PlanInspectOutcome records the result of one PLANINSPECTION+FEEDBACK pass
// for one hypothesis. A failed outcome is noted but does not stop the pipeline
// — DEEPINSPECT will work from the brief alone for that hypothesis.
type PlanInspectOutcome struct {
	Hypothesis       Hypothesis
	Content          string   // annotated file list as Markdown
	ToolsCalled      []string // tools the planner used
	FeedbackApproved bool     // true if FEEDBACK accepted the result
	Failed           bool     // true if the run errored or feedback was exhausted
	FailReason       string   // populated when Failed=true
}

// DeepInspectOutcome records the result of one DEEPINSPECT+FEEDBACK pass for
// one hypothesis. A failed outcome (Failed=true) is noted but does not stop
// the pipeline.
type DeepInspectOutcome struct {
	Hypothesis       Hypothesis
	Content          string   // the agent's Markdown analysis (may be partial if failed)
	ToolsCalled      []string // tools the agent used
	FeedbackApproved bool     // true if FEEDBACK accepted the result
	Failed           bool     // true if the run errored or feedback was exhausted
	FailReason       string   // populated when Failed=true
}

// FinalResult is the end-to-end outcome for one failing test. It is the value
// returned by Pipeline.Run and consumed by the report writer.
type FinalResult struct {
	Test         jenkins.FailedTest
	LogPath      string               // workspace-relative saved log
	Brief        string               // LOGPARSE output
	Hypotheses   []Hypothesis         // HYPOTHESIZE output
	Plans        []PlanInspectOutcome // one per hypothesis (PLANINSPECTION stage)
	DeepInspects []DeepInspectOutcome // one per hypothesis
	Summary      string               // SUMMARIZE output
	LessonsPath  string               // LESSONS handoff file (workspace-relative)
	Duration     time.Duration
}

// Context is the per-test state threaded across stages. Each stage reads the
// fields earlier stages set and fills in its own.
type Context struct {
	Test           jenkins.FailedTest
	LogPath        string               // DOWNLOAD output
	LogParsePath   string               // LOGPARSE handoff file (workspace-relative)
	Brief          string               // LOGPARSE content
	HypothesisPath string               // HYPOTHESIZE handoff file
	Hypotheses     []Hypothesis         // HYPOTHESIZE parsed output
	Plans          []PlanInspectOutcome // PLANINSPECTION+FEEDBACK results, one per hypothesis
	DeepInspects   []DeepInspectOutcome // DEEPINSPECT+FEEDBACK results
	SummaryPath    string               // SUMMARIZE handoff file
	Summary        string               // SUMMARIZE content
	LessonsPath    string               // LESSONS handoff file
}

// Stage is one step of the state machine.
type Stage interface {
	Name() State
	Run(ctx context.Context, sc *Context) error
}

// StageSpec pairs a primary LLM with the LLM used for its FEEDBACK gate.
// FeedbackLLM may equal LLM when no explicit override is configured.
// ResetCounter, if non-nil, is called at the start of each agent run to reset
// the proxy's per-run request counter (so [llm] heartbeats show #1, #2, …
// within each hypothesis rather than a monotone total). Only meaningful for
// tool-using stages (PLANINSPECTION, DEEPINSPECT).
type StageSpec struct {
	LLM          config.LLMSpec
	FeedbackLLM  config.LLMSpec
	ResetCounter func()
}

// PipelineSpec names the LLMs for every stage. The feedbacks for each stage
// use their respective FeedbackLLM from the corresponding StageSpec.
type PipelineSpec struct {
	LogParse    StageSpec
	Hypothesize StageSpec
	Plan        StageSpec
	DeepInspect StageSpec
	Summarize   StageSpec
	Lessons     StageSpec
}

// Pipeline runs the ordered stages for each test against a fixed workspace.
type Pipeline struct {
	stages     []Stage
	stateNames []State // for display
	verbose    bool
}

// New builds the full pipeline. verbose enables per-stage progress output.
// drainInterrupt, if non-nil, is called before each DEEPINSPECT attempt to
// discard queued operator messages that arrived between hypothesis runs; it is
// the InterruptController.Drain method wired in by main.
// memory is the contents of .testdiag/memory.md from prior runs (may be "").
// pauseFn, if non-nil, is called after every handoff print regardless of
// verbose; it should print "Press <ENTER> to continue..." and block until the
// user presses ENTER. When pauseFn is non-nil the handoff is printed even if
// verbose is false.
func New(cfg *config.Config, ws *workspace.Workspace, spec PipelineSpec, mode failmode.Mode, background, memory string, verbose bool, interrupt inspect.Interrupter, drainInterrupt func(), pauseFn func()) *Pipeline {
	sc := &cfg.StageConfig

	plnr := planner.New(ws, spec.Plan.LLM, background, memory, sc.PlanMaxToolIterations, sc.InspectMaxKnowledgeChars, cfg.Workspace.Mapper)
	diagnoser := diagnose.New(ws, spec.DeepInspect.LLM, mode, background, memory, sc.DeepInspectMaxToolIterations, sc.InspectMaxKnowledgeChars, cfg.Workspace.Mapper, interrupt, drainInterrupt)

	// Build feedback checkers for each stage.
	var lpFB, hFB, planFB, diFB, cFB *feedbackChecker
	if sc.LogParseMaxFeedbacks > 0 {
		lpFB = &feedbackChecker{llm: spec.LogParse.FeedbackLLM, systemPrompt: buildLogParseFeedbackPrompt(mode)}
	}
	if sc.HypothesizeMaxFeedbacks > 0 {
		hFB = &feedbackChecker{llm: spec.Hypothesize.FeedbackLLM, systemPrompt: buildHypothesizeFeedbackPrompt(mode)}
	}
	if sc.PlanMaxFeedbacks > 0 {
		planFB = &feedbackChecker{llm: spec.Plan.FeedbackLLM, systemPrompt: planInspectFeedbackPrompt}
	}
	if sc.DeepInspectMaxFeedbacks > 0 {
		diFB = &feedbackChecker{llm: spec.DeepInspect.FeedbackLLM, systemPrompt: buildDeepInspectFeedbackPrompt(mode)}
	}
	if sc.SummarizeMaxFeedbacks > 0 {
		cFB = &feedbackChecker{llm: spec.Summarize.FeedbackLLM, systemPrompt: summarizeFeedbackPrompt}
	}

	archDoc := cfg.Workspace.ArchitectureDoc

	names := []State{StateDownload, StateLogParse}
	if sc.LogParseMaxFeedbacks > 0 {
		names = append(names, StateFeedback)
	}
	names = append(names, StateHypothsize)
	if sc.HypothesizeMaxFeedbacks > 0 {
		names = append(names, StateFeedback)
	}
	names = append(names, StatePlanInspect)
	if sc.PlanMaxFeedbacks > 0 {
		names = append(names, StateFeedback)
	}
	names = append(names, StateDeepInspect)
	names = append(names, StateSummarize)
	if sc.SummarizeMaxFeedbacks > 0 {
		names = append(names, StateFeedback)
	}
	names = append(names, StateLessons)

	return &Pipeline{
		stages: []Stage{
			&downloadStage{ws: ws, verbose: verbose},
			newLogParseStage(ws, spec.LogParse.LLM, mode, lpFB, sc.LogParseMaxFeedbacks, verbose, pauseFn),
			newHypothesizeStage(ws, spec.Hypothesize.LLM, mode, archDoc, memory, hFB, sc.HypothesizeMaxFeedbacks, verbose, pauseFn),
			newPlanInspectAllStage(plnr, ws, archDoc, planFB, sc.PlanMaxFeedbacks, spec.Plan.ResetCounter, verbose, pauseFn),
			newDeepInspectAllStage(diagnoser, ws, diFB, sc.DeepInspectMaxFeedbacks, spec.DeepInspect.ResetCounter, verbose, pauseFn),
			newSummarizeStage(ws, spec.Summarize.LLM, cFB, sc.SummarizeMaxFeedbacks, verbose, pauseFn),
			newLessonsStage(ws, spec.Lessons.LLM, archDoc, verbose, pauseFn),
		},
		stateNames: names,
		verbose:    verbose,
	}
}

// stageBanner prints the white-on-red stage entry banner to stdout when
// verbose is true. label is the stage name (with optional hypothesis suffix
// for per-hypothesis stages); iter is the 1-based attempt number.
func stageBanner(verbose bool, label string, iter int) {
	if !verbose {
		return
	}
	if iter > 1 {
		fmt.Fprintf(os.Stdout, "\033[1;97;41m ENTERING %s (attempt %d) \033[0m\n", label, iter)
	} else {
		fmt.Fprintf(os.Stdout, "\033[1;97;41m ENTERING %s \033[0m\n", label)
	}
}

// States returns the pipeline stage names in order, for display.
func (p *Pipeline) States() []State {
	return p.stateNames
}

// Run drives one test through every stage in order, stopping at the first
// unrecoverable error, and returns the final result for reporting.
func (p *Pipeline) Run(ctx context.Context, test jenkins.FailedTest) (FinalResult, error) {
	start := time.Now()
	sc := &Context{Test: test}
	for _, st := range p.stages {
		if err := ctx.Err(); err != nil {
			return FinalResult{}, err
		}
		if err := st.Run(ctx, sc); err != nil {
			return FinalResult{}, fmt.Errorf("%s stage: %w", st.Name(), err)
		}
	}
	return FinalResult{
		Test:         sc.Test,
		LogPath:      sc.LogPath,
		Brief:        sc.Brief,
		Hypotheses:   sc.Hypotheses,
		Plans:        sc.Plans,
		DeepInspects: sc.DeepInspects,
		Summary:      sc.Summary,
		LessonsPath:  sc.LessonsPath,
		Duration:     time.Since(start),
	}, nil
}
