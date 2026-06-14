package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// hypothesizeStage reads the LOGPARSE brief and the optional architecture
// document, then asks an LLM to produce a ranked list of 1–N hypotheses about
// what could have caused the failure. A FEEDBACK gate checks the list; if it
// is rejected the model retries with the critique. If 0 hypotheses are
// produced (even after retries), the test is abandoned.
type hypothesizeStage struct {
	ws           *workspace.Workspace
	llm          config.LLMSpec
	archDocPath  string           // workspace-relative; may be empty
	feedback     *feedbackChecker // nil when disabled
	maxFeedbacks int
	verbose      bool
	pauseFn      func() // non-nil when -p is set; called after each handoff print
}

func newHypothesizeStage(ws *workspace.Workspace, llm config.LLMSpec, archDocPath string, fb *feedbackChecker, maxFeedbacks int, verbose bool, pauseFn func()) *hypothesizeStage {
	return &hypothesizeStage{
		ws: ws, llm: llm, archDocPath: archDocPath,
		feedback: fb, maxFeedbacks: maxFeedbacks, verbose: verbose, pauseFn: pauseFn,
	}
}

func (s *hypothesizeStage) Name() State { return StateHypothsize }

func (s *hypothesizeStage) Run(ctx context.Context, sc *Context) error {
	archDoc := s.readArchDoc()

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
			prompt = buildHypothesizePrompt(sc.Test, sc.Brief, archDoc)
		} else {
			prompt = buildHypothesizeRetryPrompt(sc.Test, sc.Brief, archDoc, prevOutput, critique)
		}
		r, err := agent.Run(ctx, prompt)
		if err != nil {
			return fmt.Errorf("agent run: %w", err)
		}
		content := strings.TrimSpace(r.Content)
		if content == "" {
			return fmt.Errorf("HYPOTHESIZE agent returned empty output for %s", sc.Test.FullName())
		}

		hypotheses := parseHypotheses(content)
		if len(hypotheses) == 0 && s.feedback == nil {
			return fmt.Errorf("HYPOTHESIZE produced 0 hypotheses for %s — cannot proceed", sc.Test.FullName())
		}

		if s.feedback == nil {
			if len(hypotheses) == 0 {
				return fmt.Errorf("HYPOTHESIZE produced 0 hypotheses for %s — cannot proceed", sc.Test.FullName())
			}
			return s.save(sc, content, hypotheses)
		}

		ok, newCritique, err := s.feedback.Check(ctx, sc.Test, content, "")
		if err != nil {
			return fmt.Errorf("feedback: %w", err)
		}
		if s.verbose {
			if ok {
				fmt.Fprintf(os.Stdout, "  HYPOTHESIZE FEEDBACK: APPROVED (%d hypothesis/es)\n", len(hypotheses))
			} else {
				fmt.Fprintf(os.Stdout, "  HYPOTHESIZE FEEDBACK: NEEDS REVISION: %s\n", newCritique)
			}
		}
		if ok {
			if len(hypotheses) == 0 {
				return fmt.Errorf("HYPOTHESIZE produced 0 parseable hypotheses for %s — cannot proceed", sc.Test.FullName())
			}
			return s.save(sc, content, hypotheses)
		}
		feedbacks++
		if feedbacks >= s.maxFeedbacks {
			return fmt.Errorf("%s: HYPOTHESIZE did not meet goals after %d feedback(s): %s",
				sc.Test.FullName(), feedbacks, newCritique)
		}
		prevOutput = content
		critique = newCritique
	}
}

func (s *hypothesizeStage) save(sc *Context, content string, hypotheses []Hypothesis) error {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rel := filepath.Join(handoffDir, sanitize(sc.Test.FullName())+".hypothesize.md")
	abs := filepath.Join(s.ws.Root(), rel)
	header := fmt.Sprintf("# Hypotheses (HYPOTHESIZE): %s\n\n", sc.Test.FullName())
	if err := os.WriteFile(abs, []byte(header+strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		return err
	}
	sc.HypothesisPath = filepath.ToSlash(rel)
	sc.Hypotheses = hypotheses
	if s.verbose || s.pauseFn != nil {
		fmt.Fprintf(os.Stdout, "--- HYPOTHESIZE handoff for %s ---\n%s\n--- end of handoff ---\n\n",
			sc.Test.FullName(), strings.TrimSpace(content))
	}
	if s.pauseFn != nil {
		s.pauseFn()
	}
	return nil
}

// readArchDoc reads the architecture document from the workspace if configured
// and the file exists. Returns empty string on any error (treated as optional).
func (s *hypothesizeStage) readArchDoc() string {
	if s.archDocPath == "" {
		return ""
	}
	abs := filepath.Join(s.ws.Root(), s.archDocPath)
	data, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	return string(data)
}

func (s *hypothesizeStage) buildAgent(test jenkins.FailedTest) (vnext.Agent, error) {
	name := "hypothesize-" + sanitize(test.FullName())
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: hypothesizeSystemPrompt,
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

// hypothesisRe matches a numbered hypothesis header in the model output.
var hypothesisRe = regexp.MustCompile(`(?m)^##\s+Hypothesis\s+(\d+)\s*:\s*(.+)$`)

// parseHypotheses extracts the numbered hypotheses from the model's output.
// It expects sections of the form:
//
//	## Hypothesis 1: <title>
//	<description text>
func parseHypotheses(content string) []Hypothesis {
	locs := hypothesisRe.FindAllStringIndex(content, -1)
	matches := hypothesisRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Hypothesis, 0, len(matches))
	for i, m := range matches {
		start := locs[i][1] // byte after the header line
		var end int
		if i+1 < len(locs) {
			end = locs[i+1][0] // start of next header
		} else {
			end = len(content)
		}
		desc := strings.TrimSpace(content[start:end])
		out = append(out, Hypothesis{
			Index:       i + 1,
			Title:       strings.TrimSpace(m[2]),
			Description: desc,
		})
	}
	return out
}

const hypothesizeSystemPrompt = `You are a distributed systems analyst. You are given an investigation brief from a log-analysis stage describing what was observed when a flaky automated test failed, and optionally an architecture document describing the system under test.

Your job is to generate a short ranked list of 1–3 hypotheses about what specific system behavior could have caused this failure. Prioritize the most likely nondeterministic mechanism.

Each hypothesis must:
- Name a specific component, code path, or interaction described in the architecture document (if provided) or implied by the brief.
- Tie back to concrete evidence in the investigation brief.
- Describe a specific nondeterministic condition: race, ordering assumption, timing window, resource limit, port or state collision, environmental variation.
- List the exact code symbols (file, class, function/method) that would prove or disprove it, and the minimal set of files an engineer would need to inspect to confirm or refute it.

Output ONLY Markdown with this exact format (no preamble, no trailing text):

## Hypothesis 1: <short title summarizing the nondeterministic mechanism>
<2–4 sentence description tying the architecture to the log evidence; state WHAT could be racing/timing-out/colliding and WHERE it likely lives in the codebase>

**Key symbols:** ` + "`<file>:<class>.<function>`" + `, … (the symbols whose implementation would confirm or refute this)
**Files to inspect:** ` + "`<file>`" + `, … (minimal set needed to confirm or refute)

## Hypothesis 2: <short title>
<description>

**Key symbols:** …
**Files to inspect:** …

(Add further hypotheses only if well supported by the evidence.)`

func buildHypothesizePrompt(test jenkins.FailedTest, brief, archDoc string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Generate hypotheses for the failing test **%s**.\n\n", test.FullName())
	b.WriteString("## Investigation brief (from LOGPARSE)\n\n")
	b.WriteString(strings.TrimSpace(brief))
	b.WriteString("\n\n")
	if strings.TrimSpace(archDoc) != "" {
		b.WriteString("## Architecture document\n\n")
		b.WriteString(strings.TrimSpace(archDoc))
		b.WriteString("\n\n")
	}
	b.WriteString("Using the brief and (if provided) the architecture document, produce a ranked list of hypotheses in the required format.")
	return b.String()
}

func buildHypothesizeRetryPrompt(test jenkins.FailedTest, brief, archDoc, prevOutput, critique string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your previous hypothesis list for **%s** was reviewed and found insufficient.\n\n", test.FullName())
	b.WriteString("## What needs to be fixed\n\n")
	b.WriteString(strings.TrimSpace(critique))
	b.WriteString("\n\n## Your previous output (for reference)\n\n")
	b.WriteString(strings.TrimSpace(prevOutput))
	b.WriteString("\n\n## Investigation brief (from LOGPARSE)\n\n")
	b.WriteString(strings.TrimSpace(brief))
	b.WriteString("\n\n")
	if strings.TrimSpace(archDoc) != "" {
		b.WriteString("## Architecture document\n\n")
		b.WriteString(strings.TrimSpace(archDoc))
		b.WriteString("\n\n")
	}
	b.WriteString("Produce an improved hypothesis list that addresses every gap listed above.")
	return b.String()
}
