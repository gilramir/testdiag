package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/jenkins"
)

// feedbackChecker runs a single tool-less LLM pass to assess whether a stage's
// output meets its quality goals. It is shared across all stages; the system
// prompt is what makes each instance stage-specific.
//
// Construct via a struct literal or the newFeedbackChecker helper:
//
//	fb := &feedbackChecker{llm: myLLM, systemPrompt: logParseFeedbackPrompt}
type feedbackChecker struct {
	llm          config.LLMSpec
	systemPrompt string
}

func newFeedbackChecker(llm config.LLMSpec, systemPrompt string) *feedbackChecker {
	return &feedbackChecker{llm: llm, systemPrompt: systemPrompt}
}

// Check assesses output against the stage's quality criteria. It returns
// ok=true when the output is acceptable, or ok=false with a critique that
// names exactly what is missing or wrong so the stage can retry.
func (f *feedbackChecker) Check(ctx context.Context, test jenkins.FailedTest, output string) (ok bool, critique string, err error) {
	name := "feedback-" + sanitize(test.FullName())
	agent, err := vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: f.systemPrompt,
			LLM: vnext.LLMConfig{
				Provider:    f.llm.Provider,
				Model:       f.llm.Model,
				BaseURL:     f.llm.BaseURL,
				APIKey:      f.llm.APIKey,
				Temperature: f.llm.Temperature,
				MaxTokens:   f.llm.MaxTokens,
			},
			Tools:   &vnext.ToolsConfig{Enabled: false},
			Memory:  &vnext.MemoryConfig{Enabled: false},
			Timeout: 5 * time.Minute,
		}).
		Build()
	if err != nil {
		return false, "", fmt.Errorf("building feedback agent: %w", err)
	}

	r, err := agent.Run(ctx, buildFeedbackPrompt(test, output))
	if err != nil {
		return false, "", fmt.Errorf("feedback agent run: %w", err)
	}

	resp := strings.TrimSpace(r.Content)
	if strings.HasPrefix(strings.ToUpper(resp), "APPROVED") {
		return true, "", nil
	}

	// Strip the "NEEDS REVISION:" header, keeping only the critique body.
	critique = resp
	if upper := strings.ToUpper(resp); strings.HasPrefix(upper, "NEEDS REVISION") {
		rest := resp[len("NEEDS REVISION"):]
		rest = strings.TrimLeft(rest, ":- \t\n")
		if rest != "" {
			critique = rest
		}
	}
	return false, critique, nil
}

func buildFeedbackPrompt(test jenkins.FailedTest, stageOutput string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review the following output for the failing test **%s**.\n\n", test.FullName())
	b.WriteString("## Output to review\n\n")
	b.WriteString(strings.TrimSpace(stageOutput))
	b.WriteString("\n\n---\n\n")
	b.WriteString("Does this output satisfy all required criteria? Respond with APPROVED or NEEDS REVISION: <critique>.")
	return b.String()
}

// ── Per-stage feedback system prompts ────────────────────────────────────────

// logParseFeedbackPrompt is the criteria for a LOGPARSE investigation brief.
const logParseFeedbackPrompt = `You are a CI investigation brief reviewer. You will be shown a brief produced by a log-analysis stage for a flaky test failure. Assess whether the brief is good enough to hand to a second engineer who will NOT see the raw log.

A good brief must satisfy ALL FOUR criteria:
1. Identify the FIRST genuine error (not downstream noise) and quote it verbatim from the log.
2. Name specific files, class/function names, modules, ports, RPC/endpoint names, or thread names — using the exact identifiers from the log — that the next engineer should investigate.
3. Offer at least one concrete flakiness hypothesis (race, ordering, timeout, resource collision, leftover state, environment condition) tied to specific log evidence.
4. Provide enough detail that the next engineer can open the right files without seeing the log.

Respond with EXACTLY ONE of:
- The single word APPROVED if all four criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear.

Output nothing else.`

// hypothesizeFeedbackPrompt is the criteria for a HYPOTHESIZE hypothesis list.
const hypothesizeFeedbackPrompt = `You are a hypothesis reviewer. You will be shown a list of hypotheses produced by a systems-analysis stage about why a flaky test failed. Assess whether the hypotheses are actionable.

A good hypothesis list must satisfy ALL THREE criteria:
1. Contains at least one hypothesis (1–3 is ideal; more than 5 is too many).
2. Each hypothesis names a specific system component or code path and ties it to evidence in the investigation brief.
3. Each hypothesis describes a plausible nondeterministic condition (race, timing, ordering, resource, environment) — not just "the code might be wrong."

Respond with EXACTLY ONE of:
- The single word APPROVED if all three criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear.

Output nothing else.`

// deepInspectFeedbackPrompt is the criteria for a DEEPINSPECT analysis.
const deepInspectFeedbackPrompt = `You are a code investigation reviewer. You will be shown an analysis produced by a deep-inspection agent that investigated one specific hypothesis about a flaky test failure. Assess whether the analysis is adequate.

A good DEEPINSPECT analysis must satisfy ALL FOUR criteria:
1. State a clear verdict: CONFIRMED, REFUTED, or INCONCLUSIVE.
2. Cite real file paths and line numbers from the workspace (not just prose assertions).
3. Identify or rule out the specific nondeterministic condition described in the hypothesis.
4. Provide enough evidence that a human engineer can independently verify the conclusion.

Respond with EXACTLY ONE of:
- The single word APPROVED if all four criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear.

Output nothing else.`

// combineFeedbackPrompt is the criteria for a COMBINE root-cause synthesis.
const combineFeedbackPrompt = `You are a root-cause report reviewer. You will be shown a combined analysis produced by a synthesis stage that read multiple hypothesis investigations and selected the best explanation for a flaky test failure. Assess whether the combined analysis is adequate.

A good combined analysis must satisfy ALL THREE criteria:
1. Names one specific root cause (the most likely hypothesis) and explains why it was chosen over the others.
2. Cites evidence from at least one DEEPINSPECT result (file paths, line numbers, or quoted code).
3. Is structured as Markdown with at least: Summary, Root Cause, Evidence, and Confidence sections.

Respond with EXACTLY ONE of:
- The single word APPROVED if all three criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear.

Output nothing else.`
