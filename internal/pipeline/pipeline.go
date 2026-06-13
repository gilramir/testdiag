// Package pipeline runs the per-test diagnosis as an explicit state machine:
//
//	DOWNLOAD    — materialize the failing test's log to .testdiag/logs/<test>.log
//	LOGPARSE    — one tool-less LLM pass that distils the log into an investigation
//	             brief (.testdiag/handoff/<test>.logparse.md)
//	FEEDBACK    — checks the brief; rejects with a critique until it meets the goal
//	             (up to StageConfig.LogParseMaxFeedbacks rejections)
//	HYPOTHESIZE — reads the brief + the optional architecture document and produces
//	             a ranked list of 1+ hypotheses (.testdiag/handoff/<test>.hypothesize.md);
//	             0 hypotheses → the test is abandoned
//	FEEDBACK    — checks the hypothesis list (up to HypothesizeMaxFeedbacks)
//	DEEPINSPECT — one agent run per hypothesis, jailed to workspace source tools;
//	             each gets its own FEEDBACK gate; a failed hypothesis is noted but
//	             does not stop the pipeline
//	COMBINE     — reads all hypotheses + DEEPINSPECT results and picks the best
//	             supported root cause (.testdiag/handoff/<test>.combine.md)
//	FEEDBACK    — checks the combined analysis (up to CombineMaxFeedbacks)
package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/diagnose"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// State names the stages of the diagnosis state machine.
type State string

const (
	StateDownload    State = "DOWNLOAD"
	StateLogParse    State = "LOGPARSE"
	StateFeedback    State = "FEEDBACK"
	StateHypothsize  State = "HYPOTHESIZE"
	StateDeepInspect State = "DEEPINSPECT"
	StateCombine     State = "COMBINE"
	StateDone        State = "DONE"
)

// Hypothesis is one candidate explanation produced by HYPOTHESIZE.
type Hypothesis struct {
	Index       int    // 1-based
	Title       string
	Description string
}

// Text returns the full Markdown text of the hypothesis (title + description),
// suitable for injecting into prompts.
func (h Hypothesis) Text() string {
	return fmt.Sprintf("### Hypothesis %d: %s\n\n%s", h.Index, h.Title, h.Description)
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
	DeepInspects []DeepInspectOutcome // one per hypothesis
	Combined     string               // COMBINE output (final root cause Markdown)
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
	DeepInspects   []DeepInspectOutcome // DEEPINSPECT+FEEDBACK results
	CombinePath    string               // COMBINE handoff file
	Combined       string               // COMBINE content
}

// Stage is one step of the state machine.
type Stage interface {
	Name() State
	Run(ctx context.Context, sc *Context) error
}

// StageSpec pairs a primary LLM with the LLM used for its FEEDBACK gate.
// FeedbackLLM may equal LLM when no explicit override is configured.
type StageSpec struct {
	LLM         config.LLMSpec
	FeedbackLLM config.LLMSpec
}

// PipelineSpec names the LLMs for every stage. The feedbacks for each stage
// use their respective FeedbackLLM from the corresponding StageSpec.
type PipelineSpec struct {
	LogParse    StageSpec
	Hypothesize StageSpec
	DeepInspect StageSpec
	Combine     StageSpec
}

// Pipeline runs the ordered stages for each test against a fixed workspace.
type Pipeline struct {
	stages     []Stage
	stateNames []State // for display
}

// New builds the full pipeline. verbose enables per-stage progress output.
func New(cfg *config.Config, ws *workspace.Workspace, spec PipelineSpec, background string, verbose bool) *Pipeline {
	sc := &cfg.StageConfig

	diagnoser := diagnose.New(ws, spec.DeepInspect.LLM, background, sc.DeepInspectMaxToolIterations)

	// Build feedback checkers for each stage.
	var lpFB, hFB, diFB, cFB *feedbackChecker
	if sc.LogParseMaxFeedbacks > 0 {
		lpFB = &feedbackChecker{llm: spec.LogParse.FeedbackLLM, systemPrompt: logParseFeedbackPrompt}
	}
	if sc.HypothesizeMaxFeedbacks > 0 {
		hFB = &feedbackChecker{llm: spec.Hypothesize.FeedbackLLM, systemPrompt: hypothesizeFeedbackPrompt}
	}
	if sc.DeepInspectMaxFeedbacks > 0 {
		diFB = &feedbackChecker{llm: spec.DeepInspect.FeedbackLLM, systemPrompt: deepInspectFeedbackPrompt}
	}
	if sc.CombineMaxFeedbacks > 0 {
		cFB = &feedbackChecker{llm: spec.Combine.FeedbackLLM, systemPrompt: combineFeedbackPrompt}
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
	names = append(names, StateDeepInspect)
	names = append(names, StateCombine)
	if sc.CombineMaxFeedbacks > 0 {
		names = append(names, StateFeedback)
	}

	return &Pipeline{
		stages: []Stage{
			&downloadStage{ws: ws},
			newLogParseStage(ws, spec.LogParse.LLM, lpFB, sc.LogParseMaxFeedbacks, verbose),
			newHypothesizeStage(ws, spec.Hypothesize.LLM, archDoc, hFB, sc.HypothesizeMaxFeedbacks, verbose),
			newDeepInspectAllStage(diagnoser, diFB, sc.DeepInspectMaxFeedbacks, verbose),
			newCombineStage(ws, spec.Combine.LLM, cFB, sc.CombineMaxFeedbacks, verbose),
		},
		stateNames: names,
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
		DeepInspects: sc.DeepInspects,
		Combined:     sc.Combined,
		Duration:     time.Since(start),
	}, nil
}
