# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`testdiag` is a Go CLI that diagnoses Jenkins test failures with an LLM. Given a
Jenkins build URL it fetches the test report, and for each failed test it runs a
per-test AgenticGoKit agent whose native tool-calling loop reads the project's
source (jailed to the checkout) to find the root cause, writing one Markdown
report per failure.

See `README.md` for the user-facing description and `plan.txt` for the original
design notes.

## Commands

```sh
go build ./...                       # build everything
go build -o testdiag .               # build the CLI binary
go vet ./...                         # vet
go test ./...                        # run all tests (none exist yet)
go test ./internal/workspace/ -run TestResolve -v   # run a single test once they exist

# Run it (needs config + a reachable LLM endpoint; see Setup in README.md):
go run . https://jenkins.example.com/job/myapp/1234/
go run . -j 8 --output ./reports <url>
```

There is currently **no test suite**; `go test ./...` reports `[no test files]`.

## Architecture

The pipeline is a fan-out: fetch failures → diagnose each independently in a
worker pool → write a report each.

- **`main.go`** — CLI parsing (`github.com/gilramir/argparse/v2`), config load,
  and `process()`, a bounded worker pool. Each failed test is fully independent,
  so workers never share agent state.
- **`internal/config`** — TOML at `~/.config/testdiag/config.toml`, with
  `TESTDIAG_*` env vars overriding the file (env always wins, for CI secrets).
  `LLM.BaseURL` + `LLM.Model` are required.
- **`internal/jenkins`** — normalizes any build/testReport URL to
  `…/api/json?depth=1`, HTTP Basic auth (user + API **token**, not password),
  parses `suites[].cases[]`, returns cases whose status is FAILED/REGRESSION/ERROR.
- **`internal/workspace`** — the security boundary. `Workspace.Resolve` jails all
  tool paths to the checkout root: absolute paths are reinterpreted relative to
  the root, and symlinks are evaluated and re-prefix-checked so they can't escape.
  Every file tool must go through this; don't add direct `os.Open` on
  model-supplied paths elsewhere.
- **`internal/tools`** — the read-only tools exposed to the model, all jailed to
  the workspace: single-file (`read_file`, `list_directory`, `count_lines`,
  `read_lines`, `grep`), tree-wide (`search_repo` recursive grep, `find_files`
  glob/substring locate), version control (`git_blame`, `git_log` — scoped to a
  jailed path via a `--` pathspec, pager disabled, output byte-capped), and
  log-oriented (`read_log` with `tail`, `grep_log` with context lines). They
  implement `v1beta.Tool` with a `JSONSchema()` so the provider calls them
  natively. `Register(ws)` registers them **once at startup** via the global
  `vnext.RegisterInternalTool` before any agent is built, so each tool is a
  single shared, **stateless** instance (workers run concurrently — never store
  per-test state on a tool). All have hard output caps (file size, line span,
  match/entry/file counts) to protect the context window.
- **`internal/diagnose`** — the core. `Diagnoser.Diagnose` maps the test, saves
  the full failure log under `<workspace>/.testdiag/logs/` (so the jailed tools
  can read it), builds a **fresh agent per test** (memory disabled, reasoning
  loop enabled, capped at `maxToolIterations`), and runs it. `prompt.go` holds
  the system prompt and assembles the first user message with a head+tail log
  excerpt (full log reachable via the tools).
- **`internal/mapping`** — **STUB.** `MapTestToSource` returns an empty path; the
  agent then finds the file itself via `list_directory`/`grep`. This is the one
  intentional placeholder — implementing the project-specific test→source mapping
  gives the agent a precise starting point. Returning an empty `SourceFile` is a
  valid, safe outcome.
- **`internal/report`** — writes one Markdown root-cause report per test into the
  output dir.
- **`internal/toolproto`** — normalizes the various native tool-call syntaxes
  open models emit (GPT-OSS Harmony, Gemma ` ```tool_code `, Mistral
  `[TOOL_CALLS]`, Nemotron `<TOOLCALL>`, Llama 3.x bare-JSON / `<|python_tag|>`,
  plus structured `tool_calls`) into the one text shape AgenticGoKit's parser
  recognizes:
  `TOOL_CALL{"name":...,"args":{...}}`. Pure functions; well covered by tests.
- **`internal/llmproxy`** — an in-process reverse proxy fronting the LLM
  endpoint. Needed because AgenticGoKit v0.5.x's OpenAI adapter does **no**
  native tool calling: it never sends a `tools` array and reads only
  `choices[].message.content`, leaving the agent to parse tool calls out of
  text. The proxy injects the workspace tools into each request and runs the
  response `content` (and any structured `tool_calls`) through `toolproto`
  before AgenticGoKit sees it. `main.go` starts it and repoints
  `cfg.LLM.BaseURL` at it when `llm.normalize_tool_calls` is set (default on).

## Key conventions

- **AgenticGoKit** is the LLM agent framework, imported as
  `vnext "github.com/agenticgokit/agenticgokit/v1beta"`. The OpenAI-compatible
  provider is wired in via a blank import in `main.go`
  (`_ ".../plugins/llm/openai"`); `provider = "openai"` with a custom `base_url`
  targets any OpenAI-API-compatible server, including local ones.
- The `AgenticGoKit/` directory in the tree is a **local reference clone only** —
  it is git-ignored and NOT used by the build; the dependency is fetched normally
  via `go.mod` (currently `v0.5.9`). Don't edit it expecting build effects.
- Module path is `github.com/gilbertr/testdiag`; Go 1.24.
- New tools exposed to the model must implement `v1beta.Tool`, be added to the
  slice in `tools.Register`, take paths only via `workspace.Resolve`, and cap
  their output size.
