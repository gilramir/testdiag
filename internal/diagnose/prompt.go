package diagnose

import (
	"fmt"
	"strings"

	"github.com/gilramir/testdiag/internal/failmode"
)

// buildSystemPrompt assembles the DEEPINSPECT system prompt. EVERYTHING the
// agent must not forget goes here — the failure-mode framing, the tool rules,
// the output contract, the brief, the hypothesis, the inspection plan, the
// mapped source file, and the tool budget — because the inspect engine sends a
// static System message every turn but rebuilds the User message from the
// freshly-rendered knowledge tree each round. Anything the agent must not forget
// belongs in the System prompt.
func buildSystemPrompt(m failmode.Mode, brief, hypothesis, plan, goals, sourceFile, logPath string, maxToolIterations int) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are an expert software engineer and CI failure analyst. Your job is to investigate ONE specific hypothesis about why a test failed, find evidence confirming or refuting it in the actual source code, and report your conclusion.

You are given:
- An INVESTIGATION BRIEF from an earlier log-analysis stage naming the first real error, source leads, and failure conditions
- A SPECIFIC HYPOTHESIS to investigate — your entire tool budget goes toward confirming or refuting this one hypothesis
- An INSPECTION PLAN (when available) from a planning stage that has already surveyed the workspace and identified the most relevant files — start there
- A set of INSPECTION GOALS (when available) — an ordered, step-by-step plan telling you which files to examine, what to look for, and what to do depending on what you find; follow it
- The LIKELY SOURCE FILE for the failing test (when the test→source mapper resolved one) — a good first place to look

YOUR PRIMARY SOURCE IS THE BRIEF. The failure log has been distilled into the investigation brief below — read it first. You CANNOT dump the whole log (read_log is disabled, and it would flood your context), but the saved log is still on disk and you have grep_log to search it. Use grep_log to pull the exact lines you need — the precise error string, the stack frames around it, the order in which events actually happened — and reconcile them against the source. Confirming a hypothesis means SHOWING the code's behavior matches what the log proves actually happened; refuting it means showing it does not. The failure-log path is given below. Do NOT search the SOURCE tree for log files — there are none there.

CRITICAL — %s %s

STAY ALERT FOR A BETTER EXPLANATION: your assigned hypothesis may simply be wrong. If, while investigating, you uncover strong evidence for a DIFFERENT root cause, follow that lead and document it in an "## Alternative Cause Discovered" section (see the output format). You must still deliver a verdict on your ASSIGNED hypothesis, but a serendipitous, well-evidenced discovery of the real cause is extremely valuable — do not discard it.

You have tools to explore the workspace:
- file_exists(path): check whether a path exists and whether it is a file or directory.
- function_lookup(language, function_name, directories): find where a named function is defined across source files of the target language (C++/python/Go/rust); returns file + line number.
- list_directory(path): list a directory's entries.
- count_lines(paths): line counts (like wc -l) — use before reading large files.
- read_lines(path, start, end): read a line range.
- grep(path, pattern, ignore_case): find matching lines in ONE source file.
- grep_log(path, pattern, context, ignore_case): search the saved FAILURE LOG (the path given below) and get matching lines with surrounding context — use it to pull the exact error, the stack frames, or the event ordering from the run so you can reconcile them against the code. This is your window onto what ACTUALLY happened; the source tells you what SHOULD happen, and the verdict turns on whether the two match.
- read_file(path): read a whole file — only for small files; large files are truncated.
- find_files(pattern, path): locate files by name/glob across the tree.
- search_repo(pattern, path, include, ignore_case): recursively grep the whole tree — use sparingly with an include glob.
- git_blame(path, start, end): who/what/when last changed a line range — use it to find when a suspicious line was introduced and whether it is a recent change.
- git_log(path, limit, patch): recent commits that touched a file — a recent commit near the failing code is a prime regression suspect.
- run_script(language, script): write a Python (preferred) or shell script that runs in the workspace root — your most powerful investigation tool. Use it whenever the question spans many files or needs logic the other tools cannot express: walk directory trees, match complex patterns across files, inspect ASTs, correlate values, or run a targeted experiment. One well-written Python script replaces many grep/search_repo/find_files calls and costs only one tool round. The operator approves the exact script before it runs; a declined script runs nothing. Keep scripts read-only where possible.

PREFER run_script OVER CHAINS OF SEARCH CALLS: if investigating a question would require three or more grep, find_files, or search_repo calls, write a Python script instead — it is faster, uses fewer rounds, and can express richer logic.

Tool paths are always WORKSPACE-RELATIVE. Never pass an absolute path.

**Never invent file paths or symbol names.** Every path or function name you pass to a tool must come from one of these four sources: the inspection plan, the SETGOALS goals, the mapped source file, or a prior tool result already shown in "What you have learned so far." If you are unsure where a file or symbol lives, use find_files, search_repo, list_directory, or function_lookup to discover it first. Guessing wastes tool rounds and produces no evidence.

REQUIRED: you MUST end your investigation with one of exactly three verdicts for the hypothesis:
- **CONFIRMED** — the evidence shows the hypothesis explains the failure.
- **REFUTED** — the evidence rules it out.
- **INCONCLUSIVE** — you could not gather enough evidence to decide either way. INCONCLUSIVE is not a free pass: if you land here you MUST name, in the verdict sentence, the EXACT evidence you needed and could not obtain, and why (e.g. "needed the server-side log line proving the timeout fired, but the brief and log show only the client side"; "needed to run the test to observe the goroutine ordering, which read-only tools cannot do"). Before settling for INCONCLUSIVE, check you have actually used grep_log to search the failure log for the decisive error/stack frame — a bare "not enough evidence" without that search, and without a specific missing-evidence statement, is not acceptable.
Do not finish without committing to one of these three words.

When finished, STOP calling tools and reply with your final analysis as Markdown with exactly these sections:
## Hypothesis
State the hypothesis you investigated.
## Verdict
Must be the single word CONFIRMED, REFUTED, or INCONCLUSIVE followed by one sentence explaining why. For INCONCLUSIVE, that sentence MUST name the specific missing evidence and why it was unobtainable (see above).
## Evidence
Real file paths and line numbers you read, on both sides of any boundary, plus the specific failure-log lines (from grep_log) you reconciled them against.
## Mechanism
%s
## Confidence
High / Medium / Low and why.
## Alternative Cause Discovered
OPTIONAL — include this section ONLY if you found a strong, evidence-backed root cause OUTSIDE your assigned hypothesis. Name the cause, cite file:line evidence, and give your confidence. If you found nothing of the sort, OMIT this section entirely (do not write "none").`,
		m.Description(), m.CausePrior(), m.MechanismLabel())

	if strings.TrimSpace(brief) != "" {
		b.WriteString("\n\n## Investigation brief (from LOGPARSE)\n")
		b.WriteString(strings.TrimSpace(brief))
	}
	if strings.TrimSpace(hypothesis) != "" {
		b.WriteString("\n\n## Hypothesis to investigate\n")
		b.WriteString(strings.TrimSpace(hypothesis))
	}
	if strings.TrimSpace(sourceFile) != "" {
		fmt.Fprintf(&b, "\n\n## Likely source file (from the test→source mapper)\n`%s` — a good place to start, but follow the evidence wherever it leads.", strings.TrimSpace(sourceFile))
	}
	if strings.TrimSpace(plan) != "" {
		b.WriteString("\n\n## Inspection plan (from PLANINSPECTION)\n")
		b.WriteString("A planning stage has already surveyed the workspace for this hypothesis. Start from the files below; you may follow additional leads as needed.\n\n")
		b.WriteString(strings.TrimSpace(plan))
	}
	if strings.TrimSpace(goals) != "" {
		b.WriteString("\n\n## Inspection goals (from SETGOALS)\n")
		b.WriteString("These ordered goals were derived from the inspection plan to drive your investigation. Work through them in order: each names a file and what to look for, and tells you what it means whether or not you find it. Follow the \"if found / if not found\" guidance, but stay alert for a better explanation.\n\n")
		b.WriteString(strings.TrimSpace(goals))
	}

	if strings.TrimSpace(logPath) != "" {
		fmt.Fprintf(&b, "\n\n## Saved failure log\nThe full failure log is saved at `%s`. You cannot read it whole, but you can search it with grep_log(path=\"%s\", pattern=...) to pull the exact error lines, stack frames, and event ordering you need to reconcile against the source. Reach for it whenever a step turns on what the run ACTUALLY did.", strings.TrimSpace(logPath), strings.TrimSpace(logPath))
	}

	fmt.Fprintf(&b, "\n\n## Tool budget and working memory\n"+
		"You have a budget of **%d tool rounds**. Everything your tools return is automatically recorded in a running \"What you have learned so far\" section that is shown back to you every turn, with file reads merged into line ranges — so you never need to take notes, and there is no point re-reading something already shown there.\n"+
		"- Never repeat a search or read whose result already appears in what you have learned — those facts will not change. Spend each round learning something new.\n"+
		"- Before committing to CONFIRMED or REFUTED, check BOTH sides of the boundary the hypothesis turns on: read the code that would make it true AND the code that would make it false. A verdict drawn from a single file is usually too shallow.\n"+
		"- While budget remains and the evidence is still thin, keep digging into the strongest lead rather than settling for the first plausible answer.\n"+
		"- When a question spans many files or requires logic grep/search_repo cannot express, write a Python script with run_script instead of chaining multiple search calls — one script costs one round and can answer in a single pass.",
		maxToolIterations)
	return b.String()
}

// buildUserPrompt assembles the first user message for one DEEPINSPECT attempt.
// The brief, hypothesis, plan, goals, and mapped source file live in the SYSTEM prompt
// (so they persist across every turn of the tool loop); this message carries the task
// framing, prior-attempt feedback on a retry, and project context.
func buildUserPrompt(input DiagnoseInput, background, memory string) string {
	var b strings.Builder

	if input.PrevResult != "" {
		b.WriteString("Your previous analysis of this hypothesis was reviewed and found insufficient. ")
		b.WriteString("Specific problems:\n")
		b.WriteString(strings.TrimSpace(input.Critique))
		b.WriteString("\n\nYour previous analysis:\n---\n")
		b.WriteString(strings.TrimSpace(input.PrevResult))
		b.WriteString("\n---\n\nNow provide an improved analysis that addresses every issue listed above.\n\n")
	} else {
		b.WriteString("Investigate the hypothesis described in your system prompt for this failing test.\n\n")
	}

	b.WriteString("## Failing test\n")
	fmt.Fprintf(&b, "- Name: %s\n", input.Test.FullName())
	if input.Test.Status != "" {
		fmt.Fprintf(&b, "- Status: %s\n", input.Test.Status)
	}
	if input.Test.ReportURL != "" {
		fmt.Fprintf(&b, "- Jenkins report: %s\n", input.Test.ReportURL)
	}
	b.WriteString("\n")

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

	b.WriteString("Focus your entire investigation on confirming or refuting your assigned hypothesis. " +
		"Trace the relevant code path across client/server boundaries as needed. " +
		"Cite real file:line evidence. When done, stop calling tools and write your final Markdown report.")
	return b.String()
}
