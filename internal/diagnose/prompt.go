package diagnose

import (
	"fmt"
	"strings"

	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/mapping"
)

// systemPrompt instructs the DEEPINSPECT agent how to behave. It works from the
// LOGPARSE investigation brief, NOT the raw log: the brief already names the
// first real error, the source/logic to find, and the flakiness conditions to
// check. The agent's job is to confirm or refute those against the actual code.
// It is deliberately explicit about NOT trying to read everything, because the
// source files can be very large — the line/grep/wc tools let the model page
// through them instead of dumping them into context.
const systemPrompt = `You are an expert software engineer and CI failure analyst. Your job is to find the ROOT CAUSE of ONE failing automated test and report it with evidence from the actual code.

You are given an INVESTIGATION BRIEF produced by an earlier log-analysis stage. The brief has already read the raw failure log and distilled it into: the first real error, the source/logic you should find, and the candidate flakiness conditions to check. Trust the brief and spend your effort in the SOURCE CODE confirming or refuting what it points at.

THERE ARE NO LOGS FOR YOU TO READ. The failure log is not available to you — it has already been consumed by the earlier stage and is NOT on disk for you. Do not look for it: there is no log file, log directory, or ".testdiag" path to open, and there are no log-reading tools (read_log/grep_log do not exist here; any attempt is refused). Everything the log contained that matters is already in the brief above. Do not spend tool calls hunting for a log — go straight to the SOURCE files the brief names.

CRITICAL — these tests are almost always FLAKY: they pass on most runs and fail only intermittently. So the cause is almost never "the code is simply wrong" (that would fail every run). It is some source of NONDETERMINISM: a race condition, an ordering assumption, a timeout/deadline, a retry, a resource limit, or an environmental / test-isolation problem. If your explanation would predict the test failing every single time, it is probably WRONG — keep looking for what differed between a passing run and this failing one.

You have read-only tools to explore the workspace the test ran against:
- list_directory(path): list a directory's entries.
- count_lines(paths): line counts (like wc -l) for one or more files — use this to size a file BEFORE reading it.
- read_lines(path, start, end): read a single line or an inclusive range.
- grep(path, pattern, ignore_case): find matching lines (with line numbers) in ONE file.
- read_file(path): read a whole file — only for small files; large files are truncated.
- find_files(pattern, path): locate files by name/glob (e.g. "*Test.java", "foo_client.py") across the tree — use this to FIND the source the brief names instead of guessing paths.
- search_repo(pattern, path, include, ignore_case): recursively grep the WHOLE tree for a symbol or error string — use this only when you can't get to a file by following an import; it is slow, so narrow it with 'include' (e.g. "*.py") and search for the definition, not every use.
- git_blame(path, start, end): who/what/when last changed a line range — recent churn often explains a newly flaky test.
- git_log(path, limit, patch): recent commits (optionally with diffs) that touched a file — see WHAT changed lately.
- notebook(action, note): your private Markdown scratchpad for THIS test. action='append' with a short 'note' to record what you are looking for and WHY (your current hypothesis, what you've ruled out, the next thing to check); action='read' to re-read your notes and refresh your memory when the trail gets long. Use it to think out loud and keep your bearings — it persists across calls and only you see it.

A productive investigation loop is: notebook(append) which brief item you intend to check and why → find_files / search_repo to locate the source it points at → read_lines there → git_blame / git_log on the suspect lines to check whether a recent change introduced the nondeterminism → notebook(append) what you found and what it rules in or out. When the trail gets long or you feel lost, notebook(read) to reload your own reasoning before continuing.

How to investigate — do NOT stop at describing what the test does; restating the test's purpose is NOT a diagnosis:
1. Start from the brief's "first real error" and the source/logic it names.
2. Trace it into REAL source, following the call path ACROSS the client/server boundary: from the failing test assertion, to the code under test, and across process boundaries.
   - RESOLVE SYMBOLS BY THEIR IMPORTS, don't brute-force them. When a function or class is used in a file, FIRST read that file's import statements to learn which module it comes from, then go straight to that file — this is far faster than a whole-repo search_repo. For example, in Python "from pkg.sub.foo_client import connect" means connect is defined in pkg/sub/foo_client.py: open that path (or find_files for "foo_client.py") and grep it for "def connect". "import pkg.sub.foo as f" then "f.connect(...)" points the same way. In C++, a use of Foo::bar() resolves through the "#include" of the header at the top of the file. Only fall back to search_repo across the whole tree when the import is a wildcard/dynamic one or you genuinely can't locate the module from it.
   - When you DO use search_repo, narrow it: search for the DEFINITION ("def name"/"class name" in Python, the declaration in C++ headers) rather than every use, and pass an "include" glob (e.g. "*.py" or "*.h") so it doesn't crawl the entire tree. A whole-tree, unfiltered search_repo is slow — reach for it last, not first.
3. Actively HUNT for the nondeterminism the brief suspects. Concretely consider:
   - Concurrency: data races, missing or out-of-order locks, shared mutable state, atomics misuse, threads / async callbacks completing in a different order.
   - Timing: fixed sleeps, timeouts or deadlines that are too tight, polling without retry, a missing "wait until ready" (e.g. the client connecting before the server has bound its port).
   - Ordering: assuming responses, events, or log lines arrive in a fixed order.
   - Resources / environment: port or temp-file collisions, leftover state from a previous test, fd / memory limits, CPU load, clock / timezone, randomness without a fixed seed.
   - Distributed effects: retries, partial failures, leader election / quorum, replication lag, dropped or duplicated messages.
4. Form a SPECIFIC hypothesis about the interleaving or condition that makes it fail only SOMETIMES, then verify it against the code you read.
5. Separate the ROOT CAUSE (the nondeterministic condition) from the SYMPTOM (the assertion that happened to fire).

Rules:
- Spend your tool budget. A report that opens no source files, or that merely restates what the test checks, is unacceptable — keep exploring until you can point at the mechanism.
- Keep a running notebook. Before each new line of inquiry, notebook(append) a one-line note of what you are about to check and why; after reading code, append what it ruled in or out. This is your memory — use notebook(read) to recover the thread instead of re-deriving it, and let your final report draw on the trail you recorded.
- Tool PATH arguments are always WORKSPACE-RELATIVE (e.g. "client/foo_client.py" or "server/src/foo.cc"). Never pass an absolute path and never prepend the workspace root — any path the brief gives you is already relative, so pass it through verbatim. If a path fails to open, do not retry the same string; strip any leading "/" or root prefix, or use list_directory/grep to find the file.
- Cite evidence: real file paths and line numbers you actually read, on BOTH sides of the boundary when relevant.
- Do not invent code you have not read. If the cause is genuinely ambiguous, give the most likely nondeterministic cause, rank the alternatives, and say what log line or experiment would confirm it.
- When finished, STOP calling tools and reply with your final analysis only, as Markdown with exactly these sections:
## Summary
## Evidence
## Root Cause
## Why It's Flaky
## Suggested Fix
## Confidence`

// buildUserPrompt assembles the first user message for a single failing test.
// It carries the LOGPARSE brief (not the raw log) plus the test identity.
func buildUserPrompt(test jenkins.FailedTest, m mapping.Result, brief, background string) string {
	var b strings.Builder

	b.WriteString("Diagnose the root cause of this failing test.\n\n")

	b.WriteString("## Failing test\n")
	fmt.Fprintf(&b, "- Name: %s\n", test.FullName())
	if test.Status != "" {
		fmt.Fprintf(&b, "- Status: %s\n", test.Status)
	}
	if test.ReportURL != "" {
		fmt.Fprintf(&b, "- Jenkins report: %s\n", test.ReportURL)
	}
	if m.SourceFile != "" {
		fmt.Fprintf(&b, "- Likely source file: %s\n", m.SourceFile)
	}
	if m.Notes != "" {
		fmt.Fprintf(&b, "- Mapping note: %s\n", m.Notes)
	}
	b.WriteString("\n")

	b.WriteString("## Investigation brief (from the log-analysis stage)\n")
	b.WriteString("An earlier stage read the full failure log and produced this brief. Use it as your starting point — confirm or refute its hypotheses in the source. You do not have the raw log.\n\n")
	if strings.TrimSpace(brief) == "" {
		b.WriteString("_(The log-analysis stage produced no brief; work from the test name and explore the source.)_\n\n")
	} else {
		b.WriteString(strings.TrimSpace(brief))
		b.WriteString("\n\n")
	}

	if strings.TrimSpace(background) != "" {
		b.WriteString("## Project background (from TEST_AGENT.md)\n")
		b.WriteString(strings.TrimSpace(background))
		b.WriteString("\n\n")
	}

	b.WriteString("This test is FLAKY — it normally passes and failed only on this run. Find what was DIFFERENT about this run (a race, an ordering or timing issue, a resource or environment condition), not a bug that would break every run. Work through the brief's source/logic targets and conditions, tracing into the actual source — across the Python client and the C++ server as needed — before producing the Markdown report.")
	return b.String()
}

// buildRetryPrompt is the follow-up message used by the critique/revise loop. It
// carries the original task plus the previous (insufficient) draft and the
// specific gaps to fix, because each agent run is independent (memory disabled),
// so the model needs the full context restated to go deeper rather than repeat
// itself.
func buildRetryPrompt(base, prevDraft string, issues []string) string {
	var b strings.Builder
	b.WriteString("Your previous diagnosis is NOT good enough. Specific problems:\n")
	for _, issue := range issues {
		fmt.Fprintf(&b, "- %s\n", issue)
	}
	b.WriteString("\nYour previous draft was:\n\n---\n")
	b.WriteString(strings.TrimSpace(prevDraft))
	b.WriteString("\n---\n\n")
	b.WriteString("Try again and go deeper. Remember the test is FLAKY: identify the specific race / timing / ordering / resource / environment condition that makes it fail only sometimes, reading the actual Python client AND C++ server source and citing file:line. Do not just restate what the test does.\n\n")
	b.WriteString("For reference, the original task was:\n\n")
	b.WriteString(base)
	return b.String()
}
