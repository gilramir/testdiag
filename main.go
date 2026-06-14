// Command testdiag diagnoses automated-test failures from a Jenkins build.
//
// Usage:
//
//	testdiag [flags] <jenkins-build-url>
//
// It fetches the build's test report (appending /api/json), finds every failed
// test, and runs each through a pipeline:
//
//	DOWNLOAD → LOGPARSE → FEEDBACK → HYPOTHESIZE → FEEDBACK →
//	[PLANINSPECTION → FEEDBACK → DEEPINSPECT → FEEDBACK] × N →
//	SUMMARIZE → FEEDBACK → LESSONS
//
// writing one Markdown report per failure.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/gilramir/argparse/v2"

	// Side-effect import: registers the OpenAI-compatible LLM provider so
	// config "provider = openai" with a custom base_url works for local servers.
	_ "github.com/agenticgokit/agenticgokit/plugins/llm/openai"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/distill"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/llmproxy"
	"github.com/gilbertr/testdiag/internal/pipeline"
	"github.com/gilbertr/testdiag/internal/report"
	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// backgroundFile is read from the workspace root and injected into every
// diagnosis as project context.
const backgroundFile = "TEST_AGENT.md"

// memoryFile is appended to by the distillation step after each test and read
// at startup to inject prior codebase knowledge into PLANINSPECTION and DEEPINSPECT.
const memoryFile = ".testdiag/memory.md"

// options holds the parsed command-line arguments.
type options struct {
	Output  string
	Debug   bool
	Verbose bool
	Pause   bool
	URL     string
	Filters []string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "testdiag: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	opts := &options{}
	ap := argparse.New(&argparse.Command{
		Description: "Diagnose Jenkins test failures with an LLM.",
		Values:      opts,
		Epilog: "Configuration is read from ~/.config/testdiag/config.toml and may be " +
			"overridden with TESTDIAG_* environment variables. The URL may be a build " +
			"or test-report URL; /api/json is appended automatically.",
	})
	ap.Add(&argparse.Argument{
		Switches: []string{"-o", "--output"},
		MetaVar:  "DIR",
		Help:     "Output directory for reports (overrides config)",
	})
	ap.Add(&argparse.Argument{
		Switches: []string{"-d", "--debug"},
		Help:     "Log the full conversation with the LLM to stderr",
	})
	ap.Add(&argparse.Argument{
		Switches: []string{"-v", "--verbose"},
		Help:     "Log stage handoffs and tool progress to stderr",
	})
	ap.Add(&argparse.Argument{
		Switches: []string{"-p", "--pause"},
		Help:     "Pause after each stage handoff; implies printing the handoff even without -v",
	})
	ap.Add(&argparse.Argument{
		Name: "url",
		Dest: "URL",
		Help: "Jenkins build (or test-report) URL",
	})
	ap.Add(&argparse.Argument{
		Name:        "filter",
		Dest:        "Filters",
		NumArgsGlob: "*",
		MetaVar:     "SUBSTRING",
		Help: "Only diagnose tests whose name contains any of these substrings " +
			"(default: all failed tests)",
	})
	ap.Parse()
	buildURL := opts.URL

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if opts.Output != "" {
		cfg.Output.Dir = opts.Output
	}
	if opts.Debug {
		cfg.Proxy.Debug = true
	}

	ws, err := workspace.New(cfg.Workspace.Root)
	if err != nil {
		return err
	}

	background := readBackground(ws.Root())
	memory := readMemory(ws.Root())

	// Register the workspace file tools once. Exclude the output directory from
	// tree searches so the agent never reads its own generated reports.
	tools.SetVerbose(opts.Verbose)
	tools.ExcludeDir(filepath.Base(cfg.Output.Dir))
	toolNames := tools.Register(ws)

	// Resolve LLMs. Only logparse and deepinspect are required; everything else
	// falls back to a sensible default.
	logparseLLM, err := cfg.LLMForStage(config.StageLogParse)
	if err != nil {
		return err
	}
	deepinspectLLM, err := cfg.LLMForStage(config.StageDeepInspect)
	if err != nil {
		return err
	}

	// Optional stage LLMs — fall back to logparse for tool-less stages,
	// deepinspect for tool-using stages.
	hypothesizeLLM := fallbackLLM(cfg, config.StageHypothsize, logparseLLM)
	planLLM := fallbackLLM(cfg, config.StagePlanInspect, deepinspectLLM)
	summarizeLLM := fallbackLLM(cfg, config.StageSummarize, logparseLLM)
	lessonsLLM := fallbackLLM(cfg, config.StageLessons, logparseLLM)
	memorizeLLM := fallbackLLM(cfg, config.StageMemoize, logparseLLM)

	// Feedback LLMs — each falls back to its primary stage's LLM.
	logparseFBLLM := fallbackLLM(cfg, config.StageLogParseFeedback, logparseLLM)
	hypothesizeFBLLM := fallbackLLM(cfg, config.StageHypothsizeFeedback, hypothesizeLLM)
	planFBLLM := fallbackLLM(cfg, config.StagePlanInspectFeedback, planLLM)
	deepinspectFBLLM := fallbackLLM(cfg, config.StageDeepInspectFeedback, deepinspectLLM)
	summarizeFBLLM := fallbackLLM(cfg, config.StageSummarizeFeedback, summarizeLLM)

	// The interrupt controller is the sole reader of os.Stdin. It muxes lines
	// between run_script confirmation prompts and DEEPINSPECT operator chat.
	ic := llmproxy.NewInterruptController()
	ic.WatchStdin()
	tools.SetStdinReader(ic.ConfirmLine)

	// Front each LLM with the in-process normalizing proxy. Stages sharing the
	// same (endpoint, tool set) reuse one proxy instance.
	pm := newProxyManager(cfg.Proxy, opts.Verbose, ic)
	defer pm.Close()
	if pm.enabled() {
		var deepTools []llmproxy.Tool
		if cfg.Proxy.InjectTools {
			deepTools = toProxyTools(tools.SchemasExcluding(tools.LogToolNames...))
		}
		// Tool-less stages share a proxy when they use the same endpoint.
		for stageName, llmPtr := range map[string]*config.LLMSpec{
			"logparse":                &logparseLLM,
			"logparse_feedback":       &logparseFBLLM,
			"hypothesize":             &hypothesizeLLM,
			"hypothesize_feedback":    &hypothesizeFBLLM,
			"planinspection_feedback": &planFBLLM,
			"deepinspect_feedback":    &deepinspectFBLLM,
			"summarize":               &summarizeLLM,
			"summarize_feedback":      &summarizeFBLLM,
			"lessons":                 &lessonsLLM,
			"memorize":                &memorizeLLM,
		} {
			if *llmPtr, err = pm.front(stageName, *llmPtr, nil); err != nil {
				return err
			}
		}
		// PLANINSPECTION uses workspace tools but no interrupt support.
		if planLLM, err = pm.front("planinspection", planLLM, deepTools); err != nil {
			return err
		}
		// DEEPINSPECT uses workspace tools and interrupt support. Always gets its
		// own proxy (the "deepinspect" key suffix ensures no sharing with PLANINSPECTION).
		if deepinspectLLM, err = pm.front("deepinspect", deepinspectLLM, deepTools); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jc := jenkins.NewClient(cfg.Jenkins.User, cfg.Jenkins.APIToken)
	failures, err := jc.FetchFailedTests(ctx, buildURL)
	if err != nil {
		return err
	}
	if len(failures) == 0 {
		fmt.Println("No failed tests found in the build report. Nothing to diagnose.")
		return nil
	}

	if len(opts.Filters) > 0 {
		failures = filterTests(failures, opts.Filters)
		if len(failures) == 0 {
			fmt.Printf("No failed tests match the given filter(s): %v\n", opts.Filters)
			return nil
		}
	}

	sc := &cfg.StageConfig
	spec := pipeline.PipelineSpec{
		LogParse: pipeline.StageSpec{
			LLM:         logparseLLM,
			FeedbackLLM: logparseFBLLM,
		},
		Hypothesize: pipeline.StageSpec{
			LLM:         hypothesizeLLM,
			FeedbackLLM: hypothesizeFBLLM,
		},
		Plan: pipeline.StageSpec{
			LLM:          planLLM,
			FeedbackLLM:  planFBLLM,
			ResetCounter: pm.resetFn("planinspection"),
		},
		DeepInspect: pipeline.StageSpec{
			LLM:          deepinspectLLM,
			FeedbackLLM:  deepinspectFBLLM,
			ResetCounter: pm.resetFn("deepinspect"),
		},
		Summarize: pipeline.StageSpec{
			LLM:         summarizeLLM,
			FeedbackLLM: summarizeFBLLM,
		},
		Lessons: pipeline.StageSpec{
			LLM: lessonsLLM,
		},
	}
	var pauseFn func()
	if opts.Pause {
		pauseFn = func() {
			fmt.Fprint(os.Stdout, "Press <ENTER> to continue...")
			ic.ConfirmLine()
			fmt.Fprintln(os.Stdout)
		}
	}
	pl := pipeline.New(cfg, ws, spec, background, memory, opts.Verbose, ic.Drain, pauseFn)
	distiller := distill.New(ws, memorizeLLM)

	fmt.Printf("Found %d failed test(s). Workspace: %s\n", len(failures), ws.Root())
	fmt.Printf("Pipeline: %v\n", pl.States())
	fmt.Printf("  LOGPARSE    -> %s (model %s, feedbacks=%d)\n",
		logparseLLM.BaseURL, logparseLLM.Model, sc.LogParseMaxFeedbacks)
	if cfg.Workspace.ArchitectureDoc != "" {
		fmt.Printf("  HYPOTHESIZE -> %s (model %s, arch=%s, feedbacks=%d)\n",
			hypothesizeLLM.BaseURL, hypothesizeLLM.Model,
			cfg.Workspace.ArchitectureDoc, sc.HypothesizeMaxFeedbacks)
	} else {
		fmt.Printf("  HYPOTHESIZE -> %s (model %s, feedbacks=%d)\n",
			hypothesizeLLM.BaseURL, hypothesizeLLM.Model, sc.HypothesizeMaxFeedbacks)
	}
	fmt.Printf("  DEEPINSPECT -> %s (model %s, tools=%v, max_iters=%d, feedbacks=%d)\n",
		deepinspectLLM.BaseURL, deepinspectLLM.Model, toolNames,
		sc.DeepInspectMaxToolIterations, sc.DeepInspectMaxFeedbacks)
	fmt.Printf("  SUMMARIZE   -> %s (model %s, feedbacks=%d)\n",
		summarizeLLM.BaseURL, summarizeLLM.Model, sc.SummarizeMaxFeedbacks)
	fmt.Printf("  LESSONS     -> %s (model %s)\n",
		lessonsLLM.BaseURL, lessonsLLM.Model)
	fmt.Printf("  MEMORIZE    -> %s (model %s)\n",
		memorizeLLM.BaseURL, memorizeLLM.Model)
	if memory != "" {
		fmt.Printf("  memory: %d byte(s) of prior codebase knowledge loaded\n", len(memory))
	}
	fmt.Printf("Diagnosing one at a time; reports -> %s\n\n", cfg.Output.Dir)

	return process(ctx, pl, distiller, failures, cfg.Output)
}

// fallbackLLM resolves the LLM for an optional stage, falling back to
// fallback when the stage has no explicit assignment.
func fallbackLLM(cfg *config.Config, stage string, fallback config.LLMSpec) config.LLMSpec {
	if spec, ok := cfg.LLMForStageOptional(stage); ok {
		return spec
	}
	return fallback
}

// process diagnoses failures one at a time, in order. Sequential execution
// keeps the output and run_script approval prompts coherent for the operator.
func process(ctx context.Context, pl *pipeline.Pipeline, d *distill.Distiller, failures []jenkins.FailedTest, out config.Output) error {
	var failed, analyzed int

	for _, test := range failures {
		if ctx.Err() != nil {
			break
		}
		res, err := pl.Run(ctx, test)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", test.FullName(), err)
			continue
		}
		if path, werr := report.Write(out.Dir, res); werr != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  ✗ %s: writing report: %v\n", test.FullName(), werr)
		} else {
			analyzed++
			fmt.Printf("  ✓ %s -> %s (%s)\n", test.FullName(), path, res.Duration.Round(1e6))
		}
		if derr := d.Distill(ctx, test); derr != nil {
			fmt.Fprintf(os.Stderr, "  warning: memorize failed for %s: %v\n", test.FullName(), derr)
		}
	}

	fmt.Printf("\nDone. %d analyzed, %d failed.\n", analyzed, failed)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("interrupted: %w", err)
	}
	if failed > 0 {
		return fmt.Errorf("%d test(s) could not be diagnosed", failed)
	}
	return nil
}

// proxyManager starts at most one normalizing LLM proxy per distinct
// (endpoint, advertised tool set) and rewrites each stage LLM's BaseURL.
type proxyManager struct {
	cfg       config.Proxy
	verbose   bool
	interrupt *llmproxy.InterruptController
	byKey     map[string]*llmproxy.Proxy
	byStage   map[string]*llmproxy.Proxy // stage name → proxy (for resetFn)
	proxies   []*llmproxy.Proxy
}

func newProxyManager(cfg config.Proxy, verbose bool, ic *llmproxy.InterruptController) *proxyManager {
	return &proxyManager{
		cfg:       cfg,
		verbose:   verbose,
		interrupt: ic,
		byKey:     map[string]*llmproxy.Proxy{},
		byStage:   map[string]*llmproxy.Proxy{},
	}
}

// resetFn returns a function that resets the request counter on the proxy
// serving the given stage. Returns a no-op if the proxy is unknown (e.g.
// when the proxy is disabled).
func (m *proxyManager) resetFn(stage string) func() {
	if px, ok := m.byStage[stage]; ok {
		return px.ResetCounter
	}
	return func() {}
}

func (m *proxyManager) enabled() bool {
	return m.cfg.NormalizeToolCalls || m.cfg.Debug || m.verbose
}

func (m *proxyManager) front(stage string, spec config.LLMSpec, proxyTools []llmproxy.Tool) (config.LLMSpec, error) {
	key := spec.BaseURL + "\x00" + toolSig(proxyTools)
	// DEEPINSPECT has interrupt support; ensure it never shares a proxy with
	// another tool-using stage (e.g. PLANINSPECTION) even on the same endpoint.
	if stage == "deepinspect" {
		key += "\x00interrupt"
	}
	px, ok := m.byKey[key]
	if !ok {
		var ic *llmproxy.InterruptController
		if stage == "deepinspect" {
			ic = m.interrupt
		}
		p, err := llmproxy.Start(spec.BaseURL, llmproxy.Options{
			Tools:     proxyTools,
			Normalize: m.cfg.NormalizeToolCalls,
			Debug:     m.cfg.Debug,
			Verbose:   m.verbose,
			Interrupt: ic,
		})
		if err != nil {
			return spec, fmt.Errorf("starting LLM proxy for %s: %w", stage, err)
		}
		m.byKey[key] = p
		m.proxies = append(m.proxies, p)
		px = p
		fmt.Printf("LLM proxy active: %s -> %s (normalize=%t, tools=%d, debug=%t)\n",
			px.BaseURL(), spec.BaseURL, m.cfg.NormalizeToolCalls, len(proxyTools), m.cfg.Debug)
	}
	m.byStage[stage] = px
	spec.BaseURL = px.BaseURL()
	return spec, nil
}

func (m *proxyManager) Close() {
	for _, p := range m.proxies {
		p.Close()
	}
}

func toolSig(ts []llmproxy.Tool) string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func toProxyTools(schemas []tools.Schema) []llmproxy.Tool {
	out := make([]llmproxy.Tool, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, llmproxy.Tool{Name: s.Name, Description: s.Description, Parameters: s.Parameters})
	}
	return out
}

func filterTests(failures []jenkins.FailedTest, substrings []string) []jenkins.FailedTest {
	var kept []jenkins.FailedTest
	for _, t := range failures {
		name := t.FullName()
		for _, s := range substrings {
			if strings.Contains(name, s) {
				kept = append(kept, t)
				break
			}
		}
	}
	return kept
}

func readBackground(root string) string {
	path := filepath.Join(root, backgroundFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", path, err)
		}
		return ""
	}
	return string(data)
}

func readMemory(root string) string {
	path := filepath.Join(root, memoryFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // missing is fine; the file is created on the first run
	}
	return string(data)
}
