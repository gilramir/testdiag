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
3. For each failure, **one at a time, in order**:
   - Maps the test to its source file (project-specific — **stubbed**).
   - Saves the full failure log into the workspace so the file tools can read it.
   - Starts a fresh per-test notebook (the agent's scratchpad) under
     `.testdiag/notes/`.
   - Builds a fresh AgenticGoKit agent (memory disabled) and runs the
     **provider's native tool-calling loop**: the LLM reads the log, then
     navigates the source with the tools below until it can explain the failure.
4. Writes one Markdown root-cause report per test under `test-diagnosis/`.

Each test is diagnosed independently — its own agent, no shared memory. They are
run sequentially (rather than in a worker pool) so the output, and especially the
`run_script` approval prompts, stay coherent for the operator instead of
interleaving many tests at once.

## Tools given to the LLM

All are **jailed to the workspace root** — the model cannot read outside the
checkout (absolute paths are reinterpreted relative to the root, and symlinks are
resolved and re-checked so they can't escape). They are AgenticGoKit *internal
tools* exposing JSON Schemas, so the provider can call them natively. Every tool
has hard output caps (file size, line span, match/entry/file counts) to protect
the context window.

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
| `read_log` | Read the saved failure log (with `tail`) |
| `grep_log` | Search the failure log (with context lines) |
| `run_script` | Write + run a shell/Python script — **only after operator approval** |
| `notebook` | Per-test Markdown scratchpad (`append` / `read`) the agent uses as working memory |

The prompt steers the model to `count_lines`/`grep`/`read_lines` rather than
dumping whole files, so large logs and large sources stay within context.

`run_script` is the one tool that writes and executes. It runs nothing until the
operator approves the exact script at a `1 = Yes / 2 = No` prompt; a decline runs
nothing. The `notebook` path is fixed per test (`.testdiag/notes/<test>.md`) and
is **not** a model argument, so the agent can only write there. A loop guard
intercepts identical repeated tool calls and nudges the model to change approach.

## Setup

```sh
go mod tidy                      # download AgenticGoKit + deps
cp config.example.toml ~/.config/testdiag/config.toml
$EDITOR ~/.config/testdiag/config.toml
```

Configuration (file + `TESTDIAG_*` env overrides; env always wins, for CI
secrets) is documented in [`config.example.toml`](config.example.toml). At
minimum set the LLM `base_url` + `model` and your Jenkins `user` + `api_token`.

Useful knobs:

- `llm.normalize_tool_calls` / `llm.inject_tools` — front the endpoint with the
  in-process proxy that rewrites open-model tool-call syntaxes into the one form
  the agent parses, and advertises the workspace tools to the model. On by
  default; see below.
- `diagnosis.max_attempts` — agent runs per test (>1 enables the critique/revise
  loop; 1 disables it).
- `diagnosis.max_tool_iterations` — tool calls allowed within one attempt.

Put a `TEST_AGENT.md` at the root of the workspace you run against; its contents
are injected into every diagnosis as background context.

## Usage

```sh
# Run from inside the build's checkout (or set workspace.root in config):
testdiag https://jenkins.example.com/job/myapp/1234/

# Override the output directory:
testdiag --output ./reports https://jenkins.example.com/job/myapp/1234/testReport/

# Filter to a subset of failures: pass one or more substrings after the URL.
# Only failed tests whose name (class.method) contains any of them are
# diagnosed; with no substrings, every failed test is processed.
testdiag https://jenkins.example.com/job/myapp/1234/ 100 LoginTest

# -d/--debug logs the full LLM conversation; -v/--verbose logs tool progress.
testdiag -v https://jenkins.example.com/job/myapp/1234/
```

## Placeholders

These are intentionally left for you to complete:

- **Test → source-file mapping** — `internal/mapping/mapping.go`
  (`MapTestToSource`). This is project-specific (language, repo layout,
  package→path rules). It currently returns an empty path, which is safe: the
  agent will locate the file itself via `list_directory`/`grep`. Implement it to
  give the agent a precise starting point.

## How tool calls reach the model

AgenticGoKit v0.5.x's OpenAI adapter does **no** native tool calling: it never
sends a `tools` array and reads only `choices[].message.content`, leaving the
agent to parse tool calls out of text. testdiag bridges this with an in-process
reverse proxy (`internal/llmproxy`) that fronts your LLM endpoint: it injects the
workspace tools into each request and runs the response through `internal/toolproto`,
which normalizes the various native tool-call syntaxes open models emit
(GPT-OSS Harmony, Gemma ` ```tool_code `, Mistral `[TOOL_CALLS]`,
Nemotron `<TOOLCALL>`, Llama 3.x bare-JSON / `<|python_tag|>`, plus structured
`tool_calls`) into the one shape the agent recognizes. `main.go` starts the proxy
and repoints `base_url` at it when `llm.normalize_tool_calls` is set.

## Layout

```
main.go                     CLI + sequential orchestration
internal/config             config file + env overrides
internal/jenkins            fetch /api/json, parse failed cases
internal/mapping            test -> source file  (STUB)
internal/workspace          path jail for the file tools
internal/tools              the workspace tools (native-schema internal tools)
internal/diagnose           per-test agent build, prompt, tool loop
internal/report             Markdown report writer
internal/llmproxy           in-process proxy fronting the LLM endpoint
internal/toolproto          normalize open-model tool-call syntaxes
```

The `AgenticGoKit/` directory in this tree is a local clone for reference only;
it is git-ignored and not used by the build (the dependency is fetched normally
via `go.mod`).
</content>
</invoke>
