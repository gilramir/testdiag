# testdiag

A CLI that diagnoses automated-test failures from a Jenkins build, using an LLM
(via [AgenticGoKit](https://github.com/agenticgokit/agenticgokit)) that can read
the project's source with file-inspection tools to find the **root cause** of
each failure.

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
   [PLANINSPECTION → FEEDBACK → DEEPINSPECT → FEEDBACK] × N → COMBINE → FEEDBACK
   ```

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
   - **COMBINE** — reads all hypotheses and DEEPINSPECT results (successful and
     failed) and picks the best-supported root cause
     (`.testdiag/handoff/<test>.combine.md`).
   - **FEEDBACK** — checks the combined analysis; retries up to
     `combine_max_feedbacks` times.

4. Writes one Markdown root-cause report per test under `test-diagnosis/`. The
   report contains the COMBINE analysis as its main body plus a per-hypothesis
   DEEPINSPECT appendix.

You can assign a **different LLM to every stage** (see [Setup](#setup)): a cheap
model can parse the log, generate hypotheses, and combine results, while a stronger
model does the source tracing. PLANINSPECTION defaults to the deepinspect LLM when
not explicitly assigned; all other optional stages default to the logparse LLM.

Each test is diagnosed independently — its own agents, no shared memory. Tests are
run sequentially so the output and the `run_script` approval prompts stay coherent
for the operator.

## Tools given to the LLM

All are **jailed to the workspace root** — the model cannot read outside the
checkout (absolute paths are reinterpreted relative to the root, and symlinks are
resolved and re-checked so they can't escape). They are AgenticGoKit *internal
tools* exposing JSON Schemas, so the provider can call them natively. Every tool
has hard output caps (file size, line span, match/entry/file counts) to protect the
context window.

| Tool | Purpose |
|------|---------|
| `read_file` | Read an entire (small) file |
| `list_directory` | List a directory's entries |
| `count_lines` | `wc -l` for one or more files |
| `read_lines` | Read a single line or an inclusive range |
| `grep` | Find matching lines (with numbers) in a file |
| `search_repo` | Recursive grep across the tree |
| `find_files` | Locate files by glob / substring |
| `git_blame` | Blame a jailed path |
| `git_log` | History for a jailed path (pager off, byte-capped) |
| `read_log` | Read the saved failure log (with `tail`) — **withheld from DEEPINSPECT** |
| `grep_log` | Search the failure log (with context lines) — **withheld from DEEPINSPECT** |
| `run_script` | Write + run a shell/Python script — **only after operator approval** |
| `notebook` | Per-hypothesis Markdown scratchpad (`append` / `read`) the agent uses as working memory |

The two log tools are not advertised to PLANINSPECTION or DEEPINSPECT and are
hard-disabled while either runs, so neither can re-read the raw log — both work from
the brief. LOGPARSE, HYPOTHESIZE, FEEDBACK, and COMBINE use no tools (their inputs
are given inline).

The prompt steers the model to `count_lines`/`grep`/`read_lines` rather than
dumping whole files, so large sources stay within context.

`run_script` runs nothing until the operator approves the exact script at a
`1 = Yes / 2 = No` prompt; a decline runs nothing. The `notebook` path is fixed per
hypothesis (`.testdiag/notes/<test>.h<N>.md`) and is **not** a model argument, so
the agent can only write there. A loop guard intercepts identical repeated tool
calls and nudges the model to change approach.

## Setup

```sh
go mod tidy                      # download AgenticGoKit + deps
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
# combine                  = "fast"
# logparse_feedback        = "fast"
# hypothesize_feedback     = "fast"
# planinspection_feedback  = "fast"
# deepinspect_feedback     = "fast"
# combine_feedback         = "fast"
```

The two required stages are `logparse` and `deepinspect`. PLANINSPECTION defaults to
the deepinspect LLM; everything else falls back to the logparse LLM when not
explicitly assigned.

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
combine_max_feedbacks                 = 2   # TESTDIAG_COMBINE_MAX_FEEDBACKS
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
# and tool progress.
testdiag -v https://jenkins.example.com/job/myapp/1234/
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

AgenticGoKit v0.5.x's OpenAI adapter does **no** native tool calling: it never
sends a `tools` array and reads only `choices[].message.content`, leaving the agent
to parse tool calls out of text. testdiag bridges this with an in-process reverse
proxy (`internal/llmproxy`) that fronts your LLM endpoint: it injects the workspace
tools into each request and runs the response through `internal/toolproto`, which
normalizes the various native tool-call syntaxes open models emit (GPT-OSS Harmony,
Gemma ` ```tool_code `, Mistral `[TOOL_CALLS]`, Nemotron `<TOOLCALL>`, Llama 3.x
bare-JSON / `<|python_tag|>`, plus structured `tool_calls`) into the one shape the
agent recognizes.

`main.go` starts the proxies and repoints each stage's `base_url` at one when
`[proxy].normalize_tool_calls` (or `--debug` / `-v`) is set. It runs at most one
proxy per distinct `(endpoint, advertised tool set)`: PLANINSPECTION and DEEPINSPECT
both advertise the **source** tools; all other stages (LOGPARSE, HYPOTHESIZE,
FEEDBACK, COMBINE) advertise **none**, and tool-less stages sharing the same endpoint
reuse one proxy instance. DEEPINSPECT always gets its own proxy even when it shares
an endpoint with PLANINSPECTION, because it has operator-interrupt support wired in.

## Layout

```
main.go                     CLI + sequential orchestration + per-stage proxies
internal/config             named LLMs, stage assignments, per-stage knobs, env overrides
internal/jenkins            fetch /api/json, parse failed cases
internal/pipeline           stage state machine and all stage implementations
  download.go               DOWNLOAD stage
  logparse.go               LOGPARSE stage (with feedback retry loop)
  hypothesize.go            HYPOTHESIZE stage (with feedback retry loop)
  planinspect.go            PLANINSPECTION-all stage (one breadth-first survey per hypothesis)
  deepinspect.go            DEEPINSPECT-all stage (one deep investigation per hypothesis)
  combine.go                COMBINE stage (with feedback retry loop)
  feedback.go               feedbackChecker + per-stage quality criteria
  pipeline.go               Pipeline, Context, FinalResult, Hypothesis, PlanInspectOutcome, DeepInspectOutcome
internal/planner            the PLANINSPECTION agent: build, prompt, one-shot tool loop
internal/diagnose           the DEEPINSPECT agent: build, prompt, one-shot tool loop
internal/mapping            test -> source file mapper
internal/workspace          path jail for the file tools
internal/tools              the workspace tools (native-schema internal tools)
internal/report             Markdown report writer (COMBINE body + per-hypothesis appendix)
internal/llmproxy           in-process proxy fronting an LLM endpoint
internal/toolproto          normalize open-model tool-call syntaxes
```

The `AgenticGoKit/` directory in this tree is a local clone for reference only;
it is git-ignored and not used by the build (the dependency is fetched normally
via `go.mod`).
