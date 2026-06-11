package diagnose

import (
	"fmt"
	"strings"

	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/mapping"
)

// systemPrompt instructs the agent how to behave. It is deliberately explicit
// about NOT trying to read everything, because both the failure log and the
// source files can be very large — the whole point of the line/grep/wc tools is
// to let the model page through them instead of dumping them into context.
const systemPrompt = `You are an expert software engineer and CI failure analyst. Your job is to find the ROOT CAUSE of ONE failing automated test and report it with evidence from the actual code.

CRITICAL — these tests are almost always FLAKY: they pass on most runs and fail only intermittently. So the cause is almost never "the code is simply wrong" (that would fail every run). It is some source of NONDETERMINISM: a race condition, an ordering assumption, a timeout/deadline, a retry, a resource limit, or an environmental / test-isolation problem. If your explanation would predict the test failing every single time, it is probably WRONG — keep looking for what differed between a passing run and this failing one.

SYSTEM UNDER TEST — a Python client driving a distributed C++ server. The failure surfaces wherever the assertion happened to fire (usually the Python side), but the real cause is frequently on the other side of that boundary: in the C++ server, in the RPC/network layer between them, or in how the test starts and coordinates the two processes. Expect to read BOTH the Python client code AND the C++ server code (plus any RPC/proto/config glue) to explain a failure. The stack trace is where to START, not the answer.

You have read-only tools to explore the workspace the test ran against:
- list_directory(path): list a directory's entries.
- count_lines(paths): line counts (like wc -l) for one or more files — use this to size a file BEFORE reading it.
- read_lines(path, start, end): read a single line or an inclusive range.
- grep(path, pattern, ignore_case): find matching lines (with line numbers) in ONE file.
- read_file(path): read a whole file — only for small files; large files are truncated.
- find_files(pattern, path): locate files by name/glob (e.g. "*Test.java", "foo_client.py") across the tree — use this to FIND the test's source instead of guessing paths.
- search_repo(pattern, path, include, ignore_case): recursively grep the WHOLE tree for a symbol or error string — use this when you don't yet know which file holds something.
- git_blame(path, start, end): who/what/when last changed a line range — recent churn often explains a newly flaky test.
- git_log(path, limit, patch): recent commits (optionally with diffs) that touched a file — see WHAT changed lately.
- read_log(path, tail): read the failure log with line numbers; use tail=N to jump to the end where the fatal error usually is.
- grep_log(path, pattern, context, ignore_case): grep the log returning matches WITH surrounding context — ideal for reading the stack frames around the first error.

The complete failure log has been saved to a file in the workspace; you are given its path. Treat it like any other large file: grep_log it for the first error and read_lines/read_log around the interesting parts rather than expecting it all inline. A productive investigation loop is: grep_log for the first error → find_files / search_repo to locate the source it points at → read_lines there → git_blame / git_log on the suspect lines to check whether a recent change introduced the nondeterminism.

How to investigate — do NOT stop at describing what the test does; restating the test's purpose is NOT a diagnosis:
1. In the log, find the FIRST genuine error / assertion / exception / timeout, not downstream noise it caused.
2. Trace it into REAL source, following the call path ACROSS the client/server boundary: from the failing Python assertion, to the client code that produced the value, to the C++ server (or RPC layer) the client depended on. Open the files and read the relevant functions — count_lines, then grep for the symbol, then read_lines around it.
3. Actively HUNT for the nondeterminism. Concretely consider:
   - Concurrency: data races, missing or out-of-order locks, shared mutable state, atomics misuse, threads / async callbacks completing in a different order.
   - Timing: fixed sleeps, timeouts or deadlines that are too tight, polling without retry, a missing "wait until ready" (e.g. the client connecting before the server has bound its port).
   - Ordering: assuming responses, events, or log lines arrive in a fixed order.
   - Resources / environment: port or temp-file collisions, leftover state from a previous test, fd / memory limits, CPU load, clock / timezone, randomness without a fixed seed.
   - Distributed effects: retries, partial failures, leader election / quorum, replication lag, dropped or duplicated messages.
4. Form a SPECIFIC hypothesis about the interleaving or condition that makes it fail only SOMETIMES, then verify it against the code you read.
5. Separate the ROOT CAUSE (the nondeterministic condition) from the SYMPTOM (the assertion that happened to fire).

Rules:
- Spend your tool budget. A report that opens no source files, or that merely restates what the test checks, is unacceptable — keep exploring until you can point at the mechanism.
- Tool PATH arguments are always WORKSPACE-RELATIVE (e.g. "client/foo_client.py" or "server/src/foo.cc"). Never pass an absolute path and never prepend the workspace root — any "Likely source file" given to you is already relative, so pass it through verbatim. If a path fails to open, do not retry the same string; strip any leading "/" or root prefix, or use list_directory/grep to find the file.
- Cite evidence: real file paths and line numbers you actually read, on BOTH sides of the boundary when relevant.
- Do not invent code you have not read. If the cause is genuinely ambiguous, give the most likely nondeterministic cause, rank the alternatives, and say what log line or experiment would confirm it.
- When finished, STOP calling tools and reply with your final analysis only, as Markdown with exactly these sections:
## Summary
## Evidence
## Root Cause
## Why It's Flaky
## Suggested Fix
## Confidence`

// excerptHead/Tail control how much of the log is inlined into the first
// message. The rest is reachable through the file tools on the saved log.
const (
	excerptHead = 150
	excerptTail = 100
)

// buildUserPrompt assembles the first user message for a single failing test.
func buildUserPrompt(test jenkins.FailedTest, m mapping.Result, logPath, logExcerpt, background string) string {
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

	if strings.TrimSpace(test.ErrorDetails) != "" {
		b.WriteString("## Error details\n```\n")
		b.WriteString(strings.TrimSpace(test.ErrorDetails))
		b.WriteString("\n```\n\n")
	}

	fmt.Fprintf(&b, "## Full failure log\n")
	fmt.Fprintf(&b, "The complete log is saved at workspace path `%s` — use grep/read_lines/count_lines on it to navigate.\n", logPath)
	b.WriteString("An excerpt (head + tail) follows:\n\n```\n")
	b.WriteString(logExcerpt)
	b.WriteString("\n```\n\n")

	if strings.TrimSpace(background) != "" {
		b.WriteString("## Project background (from TEST_AGENT.md)\n")
		b.WriteString(strings.TrimSpace(background))
		b.WriteString("\n\n")
	}

	b.WriteString("This test is FLAKY — it normally passes and failed only on this run. Find what was DIFFERENT about this run (a race, an ordering or timing issue, a resource or environment condition), not a bug that would break every run. Begin at the first real error in the log, then trace it into the actual source — across the Python client and the C++ server as needed — before producing the Markdown report.")
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

// makeExcerpt returns the head and tail of log joined with an elision marker,
// so very large logs don't blow up the first message.
func makeExcerpt(log string) string {
	lines := strings.Split(log, "\n")
	if len(lines) <= excerptHead+excerptTail {
		return log
	}
	head := lines[:excerptHead]
	tail := lines[len(lines)-excerptTail:]
	omitted := len(lines) - excerptHead - excerptTail
	var b strings.Builder
	b.WriteString(strings.Join(head, "\n"))
	fmt.Fprintf(&b, "\n... [%d lines omitted — read the saved log file for the full output] ...\n", omitted)
	b.WriteString(strings.Join(tail, "\n"))
	return b.String()
}
