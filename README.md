# testdiag

A CLI that diagnoses automated-test failures from a Jenkins build, using an LLM
that can read the project's source with file-inspection tools to find the
**root cause** of each failure.

> Status: reference implementation. It is wired end-to-end but has one
> deliberate placeholder you must fill in (see [Placeholders](#placeholders)).

## What it does

Given a Jenkins build URL:

1. Appends `/api/json` and fetches the build's test report (HTTP Basic auth with
   your Jenkins user + API token).
2. Parses the JSON to find every **failed** test case (with its stack trace and
   captured stdout/stderr).
3. For each failure, **one at a time, in order**, it runs a pipeline of stages,
   each handing off to the next through a Markdown file on disk:

   ```
   DOWNLOAD → LOGPARSE → FEEDBACK → HYPOTHESIZE → FEEDBACK →
   [PLANINSPECTION → FEEDBACK → DEEPINSPECT → FEEDBACK] × N →
   SUMMARIZE → FEEDBACK → LESSONS → MEMORIZE
   ```

   | Stage | Goal | Tools |
   |-------|------|-------|
   | DOWNLOAD | Fetch and save the failure log | — |
   | LOGPARSE | Distil log into an investigation brief | — (log given inline) |
   | FEEDBACK | Accept brief or return critique for retry | — |
   | HYPOTHESIZE | Rank 1–N nondeterminism hypotheses from brief + arch doc | — |
   | FEEDBACK | Accept hypothesis list or return critique | — |
   | PLANINSPECTION × N | Breadth-first workspace survey → annotated file list for DEEPINSPECT | workspace source tools |
   | FEEDBACK | Accept plan or return critique | — |
   | DEEPINSPECT × N | Confirm/refute hypothesis via source inspection | workspace source tools |
   | FEEDBACK | Accept result or return critique | — |
   | SUMMARIZE | Summarize each hypothesis (noting whether an inspection result exists), then identify the most likely root cause | — |
   | FEEDBACK | Accept synthesis or return critique | — |
   | LESSONS | Meta-analysis of this testdiag run; developer-facing suggestions for improving prompts, tools, and stage design | — |
   | MEMORIZE | Extract durable codebase facts → `.testdiag/memory.md` | — |

   *Workspace source tools* = `read_file`, `list_directory`, `file_exists`, `function_lookup`, `count_lines`, `read_lines`, `grep`, `search_repo`, `find_files`, `git_blame`, `git_log`, `run_script`, `notebook`. The raw-log tools (`read_log`, `grep_log`) are withheld from both PLANINSPECTION and DEEPINSPECT; those stages work from the brief alone. (In practice the inspect engine no longer advertises `notebook` — the fact tree is its working memory — but the tool still exists.)

   - **DOWNLOAD** — saves the test's full failure log under `.testdiag/logs/`.
   - **LOGPARSE** — one tool-less LLM pass over that log produces an
     **investigation brief** (`.testdiag/handoff/<test>.logparse.md`): the first
     real error, the source/logic to find, and the candidate flakiness conditions.
   - **FEEDBACK** — a second tool-less LLM pass that checks whether the brief
     meets four quality criteria. If not, LOGPARSE is retried with the critique
     attached, up to `logparse_max_feedbacks` times. Exhausting the limit
     abandons the test.
   - **HYPOTHESIZE** — reads the brief plus an optional **architecture document**
     you provide and produces a ranked list of 1–N concrete hypotheses about what
     nondeterministic condition caused the failure
     (`.testdiag/handoff/<test>.hypothesize.md`). Zero hypotheses abandons the test.
   - **FEEDBACK** — checks the hypothesis list; retries up to
     `hypothesize_max_feedbacks` times if it falls short.
   - **PLANINSPECTION × N** — one fresh agent per hypothesis, equipped with the
     same **workspace source tools** as DEEPINSPECT. It does **not** investigate
     deeply — its job is breadth-first: survey the workspace (using `find_files`,
     `search_repo`, `grep`, `read_lines`) and produce a **prioritized, annotated
     list of files** for DEEPINSPECT to examine
     (`.testdiag/handoff/<test>.h<N>.planinspect.md`). A failed plan is
     noted but does not stop the pipeline; DEEPINSPECT works from the brief alone
     in that case.
   - **FEEDBACK per PLANINSPECTION** — checks each plan; retries up to
     `planinspection_max_feedbacks` times.
   - **DEEPINSPECT × N** — one fresh agent per hypothesis, equipped with
     **workspace source tools** (jailed to the checkout). It receives both the
     hypothesis and the PLANINSPECTION file list and is instructed to start from
     those files. Each agent determines whether its hypothesis is CONFIRMED /
     REFUTED / INCONCLUSIVE. The raw log is withheld entirely. A hypothesis that
     errors or exhausts its feedback budget is marked as failed but does **not**
     stop the pipeline.
   - **FEEDBACK per DEEPINSPECT** — checks each DEEPINSPECT result independently;
     retries up to `deepinspect_max_feedbacks` times.
   - **SUMMARIZE** — for each hypothesis, writes a short paragraph: if a
     DEEPINSPECT result exists it explains what the inspector found; if not
     (inspection failed or was not run) it says so explicitly. After all
     hypothesis summaries it identifies the most likely root cause
     (`.testdiag/handoff/<test>.summarize.md`).
   - **FEEDBACK** — checks the summarized analysis; retries up to
     `summarize_max_feedbacks` times.

   - **LESSONS** — a tool-less LLM reads every handoff file written during the
     run (including the **tool logs** for PLANINSPECTION and DEEPINSPECT — compact
     per-call summaries of each tool name, arguments, and response size, stored at
     `.testdiag/handoff/<test>.h<N>.(plan|deep)inspect.tools.md`) plus the
     optional architecture document. It produces a developer-facing meta-analysis
     — **not** a user-facing report — evaluating how testdiag performed on this
     particular diagnosis and suggesting concrete improvements to the program:
     better prompts, more targeted tool designs, restructured stage handoffs, or
     anything that worked especially well and should be preserved
     (`.testdiag/handoff/<test>.lessons.md`). There is no feedback gate for LESSONS.

   - **MEMORIZE** — after the report is written, a tool-less LLM reads all the
     pipeline handoff files for that test and extracts **durable, reusable
     codebase facts** (specific file paths, function names, shared resources,
     component roles) that would help a future agent navigate the same codebase
     faster. Facts are appended to `.testdiag/memory.md`. On subsequent runs
     that file is loaded at startup and injected into every PLANINSPECTION and
     DEEPINSPECT prompt as prior knowledge. A missing or empty memory file is
     silently ignored.

4. Writes one Markdown root-cause report per test under `test-diagnosis/`. The
   report contains the SUMMARIZE analysis as its main body plus a per-hypothesis
   DEEPINSPECT appendix.

You can assign a **different LLM to every stage** (see [Setup](#setup)): a cheap
model can parse the log, generate hypotheses, summarize results, and distill memory,
while a stronger model does the source tracing. PLANINSPECTION defaults to the
deepinspect LLM when not explicitly assigned; all other optional stages (including
MEMORIZE) default to the logparse LLM.

Each test is diagnosed independently — its own agents, no shared memory. Tests are
run sequentially so the output and the `run_script` approval prompts stay coherent
for the operator.

## Tools given to the LLM

All are **jailed to the workspace root** — the model cannot read outside the
checkout (absolute paths are reinterpreted relative to the root, and symlinks are
resolved and re-checked so they can't escape). Each exposes a JSON Schema that the
diagnosis engine advertises to the model as a `tools` entry. Every tool has hard
output caps (file size, line span, match/entry/file counts) to protect the context
window.

Options below are listed **required first**, then optional ones with their
defaults. All paths are workspace-relative.

| Tool | Purpose | Options |
|------|---------|---------|
| `read_file` | Read an entire (small) file | `path` |
| `list_directory` | List a directory's entries | `path` (use `.` for the workspace root) |
| `file_exists` | Report whether a path exists and whether it is a file or a directory | `path` |
| `function_lookup` | Find where a named function is defined (returns file + line) | `language`, `function_name`, `directories` (array) |
| `count_lines` | `wc -l` for one or more files | `paths` (array) |
| `read_lines` | Read a single line or an inclusive range | `path`, `start`; `end` (defaults to `start`) |
| `grep` | Find matching lines (with numbers) in one file | `path`, `pattern`; `case_sensitive` (default `false`) |
| `search_repo` | Recursive grep across the tree (cached, paginated) | `regex`; `path` (default `.`), `include_glob`, `case_sensitive` (default `false`), `offset`, `limit` |
| `find_files` | Locate files by glob / substring; on a miss, returns same-named files elsewhere | `pattern`; `path` (default `.`), `case_sensitive` (default `false`) |
| `git_blame` | Blame a line range to find when/why a line last changed | `path`; `start`, `end` (default: a small window from `start`) |
| `git_log` | Recent commits touching a file or directory | `path` (default: whole repo), `limit` (default), `patch` (default `false`) |
| `read_log` | Read the saved failure log — **withheld from PLANINSPECTION and DEEPINSPECT** | `path`; `tail` (last N lines) |
| `grep_log` | Search the failure log with context — **withheld from PLANINSPECTION and DEEPINSPECT** | `path`, `pattern`; `context` (lines each side, default), `ignore_case` (default `false`) |
| `run_script` | Write + run a shell/Python script — **only after operator approval** | `language` (shell or Python 3), `script` |
| `notebook` | Per-hypothesis Markdown scratchpad (working memory) | `action` (`append` / `read`); `note` (required when appending) |

The two log tools are not advertised to PLANINSPECTION or DEEPINSPECT and are
hard-disabled while either runs, so neither can re-read the raw log — both work from
the brief. All other stages (LOGPARSE, HYPOTHESIZE, FEEDBACK, SUMMARIZE, LESSONS,
MEMORIZE) use no tools; their inputs are given inline.

**Tool logs** — after each PLANINSPECTION and DEEPINSPECT hypothesis run, testdiag
writes a compact tool-call log to
`.testdiag/handoff/<test>.h<N>.(plan|deep)inspect.tools.md`. Each entry records the
tool name, arguments, and a summary of the response (item count for lists, character
and line count for strings — not the full content). These logs are picked up
automatically by LESSONS and are also useful for debugging.

The prompt steers the model to `count_lines`/`grep`/`read_lines` rather than
dumping whole files, so large sources stay within context.

`run_script` runs nothing until the operator approves the exact script at a
`1 = Yes / 2 = No` prompt; a decline runs nothing. The `notebook` path is fixed per
hypothesis (`.testdiag/notes/<test>.h<N>.md`) and is **not** a model argument, so
the agent can only write there. A loop guard intercepts identical repeated tool
calls and nudges the model to change approach.

## Setup

```sh
go mod tidy                      # fetch dependencies
cp config.example.toml testdiag.toml    # workspace config (check this in)
$EDITOR testdiag.toml
cp config.example.toml ~/.config/testdiag/config.toml  # optional user overrides
$EDITOR ~/.config/testdiag/config.toml
```

### Two-level configuration

testdiag reads two config files in order; later values override earlier ones:

| Priority | File | Typical use |
|----------|------|-------------|
| 1 (lowest) | `<workspace>/testdiag.toml` | LLM endpoints, stage assignments, per-stage knobs — checked in with the repo |
| 2 | `~/.config/testdiag/config.toml` | API keys, personal overrides |
| 3 (highest) | `TESTDIAG_*` env vars | CI secrets |

The workspace root used to locate `testdiag.toml` is resolved before any config
is read: `TESTDIAG_WORKSPACE_ROOT` if set, otherwise the nearest ancestor of CWD
containing a `.git` directory, falling back to CWD itself.

Both files accept every key documented in [`config.example.toml`](config.example.toml).
At minimum: define at least one LLM under `[llms.<name>]` (with `base_url` +
`model`), assign one to `logparse` and `deepinspect` under `[stages]`, and set
your Jenkins `user` + `api_token`.

```toml
[llms.fast]
base_url = "http://localhost:1234/v1"
model    = "your-fast-model"

[llms.deep]
base_url = "http://localhost:5678/v1"
model    = "your-strong-model"

[stages]
logparse    = "fast"   # reads the log, writes the brief
deepinspect = "deep"   # gets the brief + plan + source tools, finds the root cause

# Optional: override individual stages
# planinspection           = "deep"   # surveys workspace for relevant files; defaults to deepinspect LLM
# hypothesize              = "fast"   # all others default to logparse LLM
# summarize                = "fast"
# lessons                  = "fast"   # meta-analysis; defaults to logparse LLM
# memorize                 = "fast"   # post-test distillation; defaults to logparse LLM
# logparse_feedback        = "fast"
# hypothesize_feedback     = "fast"
# planinspection_feedback  = "fast"
# deepinspect_feedback     = "fast"
# summarize_feedback       = "fast"
```

The two required stages are `logparse` and `deepinspect`. PLANINSPECTION defaults to
the deepinspect LLM; LESSONS, MEMORIZE, and all other optional stages fall back to
the logparse LLM when not explicitly assigned.

### Architecture document

HYPOTHESIZE and PLANINSPECTION can both read a document describing your system's
architecture. For HYPOTHESIZE it helps generate more targeted hypotheses; for
PLANINSPECTION it helps identify which components are most likely to be involved.
Point it at a workspace-relative path:

```toml
[workspace]
architecture_doc = "docs/architecture.md"   # or TESTDIAG_ARCHITECTURE_DOC env var
```

If the file is absent or the key is unset, HYPOTHESIZE works from the brief alone.

### Per-stage tuning

```toml
[stage_config]
logparse_max_feedbacks                = 2   # TESTDIAG_LOGPARSE_MAX_FEEDBACKS
hypothesize_max_feedbacks             = 2   # TESTDIAG_HYPOTHESIZE_MAX_FEEDBACKS
planinspection_max_feedbacks          = 1   # TESTDIAG_PLANINSPECTION_MAX_FEEDBACKS
planinspection_max_tool_iterations    = 20  # TESTDIAG_PLANINSPECTION_MAX_TOOL_ITERATIONS
deepinspect_max_feedbacks             = 1   # TESTDIAG_DEEPINSPECT_MAX_FEEDBACKS
deepinspect_max_tool_iterations       = 50  # TESTDIAG_DEEPINSPECT_MAX_TOOL_ITERATIONS
summarize_max_feedbacks                 = 2   # TESTDIAG_SUMMARIZE_MAX_FEEDBACKS
```

Set any `*_max_feedbacks` to `0` to disable feedback for that stage.
PLANINSPECTION has a lower tool-iteration budget than DEEPINSPECT by design — it
is a breadth-first survey, not a deep investigation.

Per-LLM secrets can come from `TESTDIAG_LLM_<NAME>_API_KEY` /
`TESTDIAG_LLM_<NAME>_BASE_URL` / `TESTDIAG_LLM_<NAME>_MODEL`.

Put a `TEST_AGENT.md` at the root of the workspace you run against; its contents
are injected into every DEEPINSPECT as background context.

## Usage

```sh
# Run from inside the build's checkout (or set workspace.root in config):
testdiag https://jenkins.example.com/job/myapp/1234/

# Override the output directory:
testdiag --output ./reports https://jenkins.example.com/job/myapp/1234/testReport/

# Filter to a subset of failures: pass one or more substrings after the URL.
# Only tests whose full name (class.method) contains any substring are diagnosed.
testdiag https://jenkins.example.com/job/myapp/1234/ LoginTest

# -d/--debug logs the full LLM conversation; -v/--verbose logs stage handoffs
# and tool progress; -p/--pause prints each handoff and waits for ENTER before
# the next stage (useful for reviewing intermediate results).
testdiag -v https://jenkins.example.com/job/myapp/1234/
testdiag -p https://jenkins.example.com/job/myapp/1234/

# By default testdiag assumes the failure is FLAKY (intermittent) and steers every
# stage toward nondeterministic causes (races, timing, ordering). If the test
# actually fails on EVERY run, pass --always-fails so the stages instead hunt for a
# deterministic defect or recent regression.
testdiag --always-fails https://jenkins.example.com/job/myapp/1234/
```

## Test → source-file mapping

Both PLANINSPECTION and DEEPINSPECT work better when they know which source file
the failing test lives in. You can supply a mapper executable that performs this
translation:

```toml
[workspace]
mapper = "/path/to/my-test-mapper"   # or TESTDIAG_MAPPER env var
```

testdiag calls it as:

```sh
my-test-mapper "com.example.FooTest.testBar"
```

The executable should print the workspace-relative source file path on stdout
(e.g. `src/main/java/com/example/FooTest.java`) and exit 0. The subprocess runs
with the workspace root as its working directory, so relative paths and workspace
files are accessible. Anything the mapper writes to stderr is passed through.

When `mapper` is empty, the test returns nothing, or the mapper exits non-zero,
PLANINSPECTION and DEEPINSPECT receive no source-file hint and locate the file
themselves via the directory/grep tools. A mapper failure prints a warning but does
not abort the diagnosis.

## How tool calls reach the model

testdiag drives the tool-calling conversation itself, so it works with any
OpenAI-API-compatible server regardless of whether the model emits native
`tool_calls` or one of the open-model text syntaxes. The tool-using stages
(PLANINSPECTION, DEEPINSPECT) run the loop in `internal/inspect`: each turn it sends
the model a system prompt plus the accumulated **fact tree** and the workspace
tools' JSON schemas, then runs the reply through `internal/toolproto`, which
normalizes the various native tool-call syntaxes (GPT-OSS Harmony, Gemma
` ```tool_code `, Mistral `[TOOL_CALLS]`, Nemotron `<TOOLCALL>`, Llama 3.x bare-JSON
/ `<|python_tag|>`, plus structured `tool_calls`) into one canonical shape, executes
the calls, folds the results into the tree, and repeats. See `CLAUDE.md` (the
`internal/inspect` and `internal/knowledge` notes) for the design.

The tool-less stages (LOGPARSE, HYPOTHESIZE, SUMMARIZE, LESSONS, FEEDBACK) make a
single completion call. `internal/llmproxy` is a now-vestigial reverse proxy that
`main.go` still optionally points the tool-less stages at for `--debug` conversation
logging; the tool-using stages bypass it.

## Layout

```
main.go                     CLI + sequential orchestration
internal/config             named LLMs, stage assignments, per-stage knobs, env overrides
internal/jenkins            fetch /api/json, parse failed cases
internal/pipeline           stage state machine and all stage implementations
  download.go               DOWNLOAD stage
  logparse.go               LOGPARSE stage (with feedback retry loop)
  hypothesize.go            HYPOTHESIZE stage (with feedback retry loop)
  planinspect.go            PLANINSPECTION-all stage (one breadth-first survey per hypothesis)
  deepinspect.go            DEEPINSPECT-all stage (one deep investigation per hypothesis)
  summarize.go              SUMMARIZE stage (with feedback retry loop)
  lessons.go                LESSONS stage (tool-less meta-analysis, no feedback gate)
  feedback.go               feedbackChecker + per-stage quality criteria
  pipeline.go               Pipeline, Context, FinalResult, Hypothesis, PlanInspectOutcome, DeepInspectOutcome
internal/inspect            the LLM client (Complete) + tool-loop engine + result ingest
internal/knowledge          the fact tree the tool loop accumulates and renders each turn
internal/planner            PLANINSPECTION stage layer (builds + runs an inspect.Engine)
internal/diagnose           DEEPINSPECT stage layer (builds + runs an inspect.Engine)
internal/distill            post-test MEMORIZE step: extract codebase facts → .testdiag/memory.md
internal/mapping            test -> source file mapper
internal/workspace          path jail for the file tools
internal/tools              the workspace tools (JSON-schema + Execute) + dispatch registry + tool-call log
internal/report             Markdown report writer (SUMMARIZE body + per-hypothesis appendix)
internal/llmproxy           vestigial proxy (debug logging) + the operator InterruptController
internal/toolproto          normalize open-model tool-call syntaxes
```
