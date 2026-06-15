package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/gilramir/testdiag/internal/config"
	"github.com/gilramir/testdiag/internal/failmode"
	"github.com/gilramir/testdiag/internal/inspect"
	"github.com/gilramir/testdiag/internal/jenkins"
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
// toolLog is the formatted tool-call log from the stage that produced output;
// pass an empty string for tool-less stages.
func (f *feedbackChecker) Check(ctx context.Context, test jenkins.FailedTest, output string, toolLog string) (ok bool, critique string, err error) {
	raw, err := inspect.Complete(ctx, f.llm, f.systemPrompt, buildFeedbackPrompt(test, output, toolLog))
	if err != nil {
		return false, "", fmt.Errorf("feedback completion: %w", err)
	}

	resp := strings.TrimSpace(raw)
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

func buildFeedbackPrompt(test jenkins.FailedTest, stageOutput string, toolLog string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review the following output for the failing test **%s**.\n\n", test.FullName())
	b.WriteString("## Output to review\n\n")
	b.WriteString(strings.TrimSpace(stageOutput))
	b.WriteString("\n\n")
	if strings.TrimSpace(toolLog) != "" {
		b.WriteString("## Tool calls made during this stage\n\n")
		b.WriteString(strings.TrimSpace(toolLog))
		b.WriteString("\n\n")
	}
	b.WriteString("---\n\n")
	b.WriteString("Does this output satisfy all required criteria? Respond with APPROVED or NEEDS REVISION: <critique>.")
	return b.String()
}

// ── Per-stage feedback system prompts ────────────────────────────────────────

// buildLogParseFeedbackPrompt is the criteria for a LOGPARSE investigation
// brief. The flakiness criterion adapts to the failure mode.
func buildLogParseFeedbackPrompt(m failmode.Mode) string {
	return fmt.Sprintf(`You are a CI investigation brief reviewer. You will be shown a brief produced by a log-analysis stage for a %s. Assess whether the brief is good enough to hand to a second engineer who will NOT see the raw log.

A good brief must satisfy ALL FOUR criteria:
1. Identify the FIRST genuine error (not downstream noise) and quote it verbatim from the log.
2. Name specific files, class/function names, modules, ports, RPC/endpoint names, or thread names — using the exact identifiers from the log — that the next engineer should investigate.
3. Offer at least one concrete hypothesis that %s, tied to specific log evidence.
4. Provide enough detail that the next engineer can open the right files without seeing the log.

Respond with EXACTLY ONE of:
- The single word APPROVED if all four criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear.

Output nothing else.`, m.ShortLabel(), m.FeedbackConditionCriterion())
}

// buildHypothesizeFeedbackPrompt is the criteria for a HYPOTHESIZE list. The
// mechanism criterion adapts to the failure mode.
func buildHypothesizeFeedbackPrompt(m failmode.Mode) string {
	return fmt.Sprintf(`You are a hypothesis reviewer. You will be shown a list of hypotheses produced by a systems-analysis stage about why a %s failed. Assess whether the hypotheses are actionable.

A good hypothesis list must satisfy ALL FIVE criteria:
1. Contains at least one hypothesis (1–3 is ideal; more than 5 is too many).
2. Each hypothesis names a specific system component or code path and ties it to evidence in the investigation brief.
3. Each hypothesis %s.
4. Each hypothesis includes a "Key symbols" line listing specific file:class.function identifiers whose implementation would confirm or refute it.
5. Each hypothesis includes a "Files to inspect" line listing the minimal set of files needed to confirm or refute it.

Respond with EXACTLY ONE of:
- The single word APPROVED if all five criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear.

Output nothing else.`, m.ShortLabel(), m.FeedbackConditionCriterion())
}

// planInspectFeedbackPrompt is the criteria for a PLANINSPECTION output.
const planInspectFeedbackPrompt = `You are reviewing an inspection plan produced by a planning stage. The plan lists workspace source files that a subsequent deep-inspection agent should examine to investigate a specific hypothesis about a failing test.

A good inspection plan must satisfy ALL FOUR criteria:
1. Lists at least two specific workspace-relative file paths with a note for each.
2. Each entry explains WHY the file is relevant to the specific hypothesis — not just "this is a source file."
3. Entries are prioritized so the most critical files appear first.
4. Every listed file path is a real workspace file. Using the tool call log, flag any path that the planning agent never confirmed exists (e.g. via file_exists, find_files, or function_lookup) — guessed paths are not acceptable.

A tool call log may be provided showing what the planning agent actually searched. When present, also assess:
- Whether the file paths in the plan were actually located by the tool calls (not just guessed from the brief).
- Whether tool calls surfaced candidate files that the plan omitted without explanation.
- Whether the search patterns used were appropriate for the hypothesis (e.g., searched for the right symbols, paths, or keywords).
Cite specific tool call numbers in your critique when the log reveals a gap.

Respond with EXACTLY ONE of:
- The single word APPROVED if all criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear (cite tool call numbers when relevant).

Output nothing else.`

// buildDeepInspectFeedbackPrompt is the criteria for a DEEPINSPECT analysis. The
// mechanism criterion adapts to the failure mode. A serendipitously discovered
// alternative cause is welcomed, not penalized.
func buildDeepInspectFeedbackPrompt(m failmode.Mode) string {
	condition := "the specific nondeterministic condition described in the hypothesis"
	if m.AlwaysFails {
		condition = "the specific deterministic defect described in the hypothesis"
	}
	return fmt.Sprintf(`You are a code investigation reviewer. You will be shown an analysis produced by a deep-inspection agent that investigated one specific hypothesis about a %s. Assess whether the analysis is adequate.

A good DEEPINSPECT analysis must satisfy ALL FOUR criteria:
1. The ## Verdict section must begin with exactly one of the words CONFIRMED, REFUTED, or INCONCLUSIVE (in all caps). An absent, ambiguous, or hedged verdict fails this criterion.
2. Cite real file paths and line numbers from the workspace (not just prose assertions).
3. Identify or rule out %s.
4. Provide enough evidence that a human engineer can independently verify the conclusion.

If the analysis includes an "## Alternative Cause Discovered" section, treat it as a BONUS — it must not be penalized, and it does not need to be fully proven. Only judge it harshly if it contradicts the cited evidence.

A tool call log may be provided showing what the deep-inspection agent actually read. When present, also assess:
- Whether the agent read the files it cited as evidence (cross-check citations against the log).
- Whether tool results that pointed to a relevant code path were followed up or dropped.
- Whether the verdict is consistent with what the tool calls actually surfaced (e.g., CONFIRMED without reading the relevant file is suspect).
- Whether the investigation stopped too early — if the log shows only a few reads before the conclusion, the evidence may be shallow.
Cite specific tool call numbers in your critique when the log reveals a gap.

Respond with EXACTLY ONE of:
- The single word APPROVED if all four criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear (cite tool call numbers when relevant).

Output nothing else.`, m.ShortLabel(), condition)
}

// summarizeFeedbackPrompt is the criteria for a SUMMARIZE output.
const summarizeFeedbackPrompt = `You are a diagnosis-summary reviewer. You will be shown a summary produced by the SUMMARIZE stage that reviewed a set of hypotheses about a failing test. Assess whether the summary is adequate.

A good SUMMARIZE output must satisfy ALL FOUR criteria:
1. Contains a section for EVERY hypothesis, either explaining what the inspection found or explicitly stating that no result was available.
2. Contains an "Alternative Causes Discovered" section that summarizes any alternative cause a deep-inspection reported, or says "None." if there were none.
3. The Most Likely Root Cause section names a cause ONLY if it is well-supported — either a hypothesis that was CONFIRMED, or an alternative cause reported with concrete file:line evidence. Otherwise it must say so explicitly and must NOT name a winner or speculate.
4. Does not invent inspection findings — summaries of hypotheses with no result must say so rather than speculating.

Respond with EXACTLY ONE of:
- The single word APPROVED if all four criteria are met, OR
- NEEDS REVISION: followed by a concise bulleted list of exactly what is missing or unclear.

Output nothing else.`
