package diagnose

import (
	"fmt"
	"strings"

	"github.com/gilbertr/testdiag/internal/mapping"
)

// systemPromptBase instructs the DEEPINSPECT agent. It is the static part;
// the brief, hypothesis, and tool budget are appended in buildSystemPrompt so
// they survive AGK's continuation loop (which preserves System across every
// tool iteration but replaces User with "Previous response + tool results").
const systemPromptBase = `You are an expert software engineer and CI failure analyst. Your job is to investigate ONE specific hypothesis about why a test failed, find evidence confirming or refuting it in the actual source code, and report your conclusion.

You are given:
- An INVESTIGATION BRIEF from an earlier log-analysis stage naming the first real error, source leads, and flakiness conditions
- A SPECIFIC HYPOTHESIS to investigate — your entire tool budget goes toward confirming or refuting this one hypothesis
- An INSPECTION PLAN (when available) from a planning stage that has already surveyed the workspace and identified the most relevant files — start there

THERE ARE NO LOGS FOR YOU TO READ. The failure log has already been consumed and is NOT available. Everything useful from the log is in the brief. Do not look for log files.

CRITICAL — these tests are FLAKY: they pass on most runs and fail only intermittently. The cause is almost never "the code is simply wrong" (that would fail every run). It is some source of NONDETERMINISM: a race, ordering assumption, timeout, resource limit, or environmental condition. If your explanation would predict the test always failing, keep looking.

You have read-only tools to explore the workspace:
- list_directory(path): list a directory's entries.
- count_lines(paths): line counts (like wc -l) — use before reading large files.
- read_lines(path, start, end): read a line range.
- grep(path, pattern, ignore_case): find matching lines in ONE file.
- read_file(path): read a whole file — only for small files; large files are truncated.
- find_files(pattern, path): locate files by name/glob across the tree.
- search_repo(pattern, path, include, ignore_case): recursively grep the whole tree — use sparingly with an include glob.
- git_blame(path, start, end): who/what/when last changed a line range.
- git_log(path, limit, patch): recent commits that touched a file.
- notebook(action, note): your private scratchpad. Use append before each new inquiry (what you are checking and why); use read to refresh when the trail gets long.
- run_script(script, language): run a short shell/Python script with operator approval — for targeted experiments only.

Tool paths are always WORKSPACE-RELATIVE. Never pass an absolute path.

When finished, STOP calling tools and reply with your final analysis as Markdown with exactly these sections:
## Hypothesis
State the hypothesis you investigated.
## Verdict
CONFIRMED, REFUTED, or INCONCLUSIVE — with one sentence explaining why.
## Evidence
Real file paths and line numbers you read, on both sides of any boundary.
## Mechanism
The specific nondeterministic condition (race / timing / ordering / resource / environment) — or why none applies if REFUTED.
## Confidence
High / Medium / Low and why.`

// buildSystemPrompt appends the brief, hypothesis, and tool budget to the
// static instructions so the agent always has them in scope regardless of how
// many tool iterations have elapsed.
func buildSystemPrompt(brief, hypothesis string, maxToolIterations int) string {
	var b strings.Builder
	b.WriteString(systemPromptBase)
	if strings.TrimSpace(brief) != "" {
		b.WriteString("\n\n## Investigation brief (from LOGPARSE)\n")
		b.WriteString(strings.TrimSpace(brief))
	}
	if strings.TrimSpace(hypothesis) != "" {
		b.WriteString("\n\n## Hypothesis to investigate\n")
		b.WriteString(strings.TrimSpace(hypothesis))
	}
	fmt.Fprintf(&b, "\n\n## Tool budget\n"+
		"You have a budget of **%d tool calls**. Spend it wisely:\n"+
		"- If you already have `search_repo` results for a regex in your notebook, reuse them — do not repeat the same search.\n"+
		"- Stop as soon as you have a CONFIRMED or REFUTED verdict; do not keep calling tools once the answer is clear.\n"+
		"- Reserve the last 1–2 calls for the notebook read and your final synthesis if needed.",
		maxToolIterations)
	return b.String()
}

// buildUserPrompt assembles the first user message for one DEEPINSPECT attempt.
// When input.PrevResult and input.Critique are set this is a feedback-triggered
// retry: the prior draft and the critique are prepended so the agent knows
// exactly what to improve.
func buildUserPrompt(input DiagnoseInput, m mapping.Result, background, memory string) string {
	var b strings.Builder

	if input.PrevResult != "" {
		b.WriteString("Your previous analysis of this hypothesis was reviewed and found insufficient. ")
		b.WriteString("Specific problems:\n")
		b.WriteString(strings.TrimSpace(input.Critique))
		b.WriteString("\n\nYour previous analysis:\n---\n")
		b.WriteString(strings.TrimSpace(input.PrevResult))
		b.WriteString("\n---\n\nNow provide an improved analysis that addresses every issue listed above.\n\n")
	} else {
		b.WriteString("Investigate the following hypothesis about this failing test.\n\n")
	}

	b.WriteString("## Failing test\n")
	fmt.Fprintf(&b, "- Name: %s\n", input.Test.FullName())
	if input.Test.Status != "" {
		fmt.Fprintf(&b, "- Status: %s\n", input.Test.Status)
	}
	if input.Test.ReportURL != "" {
		fmt.Fprintf(&b, "- Jenkins report: %s\n", input.Test.ReportURL)
	}
	if m.SourceFile != "" {
		fmt.Fprintf(&b, "- Likely source file: %s\n", m.SourceFile)
	}
	b.WriteString("\n")

	b.WriteString("## Your hypothesis (repeated from system prompt for clarity)\n")
	b.WriteString(strings.TrimSpace(input.Hypothesis))
	b.WriteString("\n\n")

	if strings.TrimSpace(input.Plan) != "" {
		b.WriteString("## Inspection plan (from PLANINSPECTION)\n")
		b.WriteString("A planning stage has already surveyed the workspace for this hypothesis. ")
		b.WriteString("Start your investigation from the files listed below; you may follow additional leads as needed.\n\n")
		b.WriteString(strings.TrimSpace(input.Plan))
		b.WriteString("\n\n")
	}

	if strings.TrimSpace(memory) != "" {
		b.WriteString("## Prior codebase knowledge (from past investigations)\n\n")
		b.WriteString(strings.TrimSpace(memory))
		b.WriteString("\n\n")
	}

	if strings.TrimSpace(background) != "" {
		b.WriteString("## Project background (from TEST_AGENT.md)\n")
		b.WriteString(strings.TrimSpace(background))
		b.WriteString("\n\n")
	}

	b.WriteString("Focus your entire investigation on confirming or refuting the hypothesis above. " +
		"Trace the relevant code path across client/server boundaries as needed. " +
		"Cite real file:line evidence. When done, stop calling tools and write your final Markdown report.")
	return b.String()
}
