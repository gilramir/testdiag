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

// feedbackChecker runs a single tool-less LLM pass to assess whether a LOGPARSE
// brief meets the investigation goals. It is used by logParseStage to gate
// hand-off: if the brief is insufficient it returns a specific critique so
// LOGPARSE can be retried with that feedback.
type feedbackChecker struct {
	llm config.LLMSpec
}

func newFeedbackChecker(llm config.LLMSpec) *feedbackChecker {
	return &feedbackChecker{llm: llm}
}

// Check assesses brief against the LOGPARSE quality criteria. It returns
// ok=true when the brief is acceptable, or ok=false with a critique that names
// exactly what is missing or wrong.
func (f *feedbackChecker) Check(ctx context.Context, test jenkins.FailedTest, brief string) (ok bool, critique string, err error) {
	name := "feedback-" + sanitize(test.FullName())
	agent, err := vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: feedbackSystemPrompt,
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

	r, err := agent.Run(ctx, buildFeedbackPrompt(test, brief))
	if err != nil {
		return false, "", fmt.Errorf("feedback agent run: %w", err)
	}

	resp := strings.TrimSpace(r.Content)
	if strings.HasPrefix(strings.ToUpper(resp), "APPROVED") {
		return true, "", nil
	}

	// Strip the "NEEDS REVISION:" header, keeping just the critique body.
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

// feedbackSystemPrompt instructs the FEEDBACK model. Its only job is to decide
// whether the brief is good enough to hand to the next stage.
const feedbackSystemPrompt = `You are a CI investigation brief reviewer. You will be shown an investigation brief produced by a log-analysis step for a flaky test failure. Your job is to assess whether the brief is good enough to hand to a second engineer who will NOT see the raw log.

A good brief must satisfy ALL FOUR criteria:
1. Identify the FIRST genuine error (not downstream noise) and quote it verbatim from the log.
2. Name specific files, class/function names, modules, ports, RPC/endpoint names, or thread names — using the exact identifiers from the log — that the next engineer should investigate.
3. Offer at least one concrete flakiness hypothesis (race, ordering, timeout, resource collision, leftover state, environment condition) tied to specific log evidence (a line, a timestamp gap, a stack frame, an ordering).
4. Provide enough detail that the next engineer can open the right files without seeing the log.

Respond with EXACTLY ONE of:
- The single word APPROVED if all four criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear (name the criterion and state specifically what is lacking).

Output nothing else.`

func buildFeedbackPrompt(test jenkins.FailedTest, brief string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review the following investigation brief for the failing test **%s**.\n\n", test.FullName())
	b.WriteString("## Investigation brief to review\n\n")
	b.WriteString(strings.TrimSpace(brief))
	b.WriteString("\n\n---\n\n")
	b.WriteString("Does this brief satisfy all four criteria? Respond with APPROVED or NEEDS REVISION: <critique>.")
	return b.String()
}
