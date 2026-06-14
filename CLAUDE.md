# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`testdiag` is a Go CLI that diagnoses Jenkins test failures with an LLM. Given a
Jenkins build URL it fetches the test report, and for each failed test it runs a
**state machine of stages**, writing one Markdown report per failure:

```
DOWNLOAD → LOGPARSE → FEEDBACK → HYPOTHESIZE → FEEDBACK →
[PLANINSPECTION → FEEDBACK → DEEPINSPECT → FEEDBACK] × N →
SUMMARIZE → FEEDBACK → LESSONS
```

- **DOWNLOAD** — save the raw failure log to disk
- **LOGPARSE** — tool-less LLM pass that distils the **entire** log into an investigation brief (the whole log is inlined, not just head+tail, since later lines often carry the decisive clue; only a log that would overflow the model's context window is trimmed)
- **FEEDBACK** — tool-less LLM gate: accepts the brief or rejects it with a critique; LOGPARSE retries with the critique up to `logparse_max_feedbacks` times
- **HYPOTHESIZE** — tool-less LLM pass that reads the brief plus an optional architecture document and produces a ranked list of 1–N hypotheses; 0 hypotheses abandons the test
- **FEEDBACK** — gate on the hypothesis list (`hypothesize_max_feedbacks`)
- **PLANINSPECTION × N** — one fresh tool-using agent per hypothesis; breadth-first workspace survey that produces a prioritized, annotated file list for DEEPINSPECT to follow; soft-fails per hypothesis. Runs on the **own tool-loop engine** (`internal/inspect`), not AgenticGoKit — see the inspect/knowledge note below.
- **FEEDBACK per PLANINSPECTION** — gate on each plan (`planinspection_max_feedbacks`); also a deterministic Go gate that rejects any plan listing files that do not exist in the workspace (see `internal/pipeline/planinspect.go`)
- **DEEPINSPECT × N** — one fresh agent per hypothesis, jailed to the workspace source tools; investigates whether that hypothesis is CONFIRMED / REFUTED / INCONCLUSIVE. Also runs on the `internal/inspect` engine.
- **FEEDBACK per DEEPINSPECT** — gate on each DEEPINSPECT result (`deepinspect_max_feedbacks`); a failed hypothesis is soft-failed (noted but does not stop the pipeline)
- **SUMMARIZE** — tool-less LLM pass; for each hypothesis writes a paragraph explaining what DEEPINSPECT found (or explicitly noting no result if the inspection failed or was not run), then adds a "Most Likely Root Cause" verdict
- **FEEDBACK** — gate on the summary (`summarize_max_feedbacks`)
- **LESSONS** — tool-less LLM pass; reads all handoff files + tool logs produced during the run (including `.testdiag/handoff/<test>.h<N>.(plan|deep)inspect.tools.md`) plus the optional architecture document; produces developer-facing meta-analysis: prompt quality, tool usage patterns, stage design suggestions, and what worked well; no feedback gate; output at `.testdiag/handoff/<test>.lessons.md`

Each stage hands off to the next through a Markdown file on disk (`.testdiag/handoff/`) and each LLM can be configured independently. Different LLMs can be assigned to different stages so a cheap model can parse the log while a stronger one does the source tracing.

**Two execution models.** The tool-less stages (LOGPARSE, HYPOTHESIZE, SUMMARIZE, LESSONS, and every FEEDBACK gate) run on **AgenticGoKit** fronted by the normalizing `internal/llmproxy`. The two **tool-using** stages (PLANINSPECTION, DEEPINSPECT) do NOT: they run on our **own** tool-loop engine, `internal/inspect`, which talks to the model server directly and accumulates results into a deduplicated **fact tree** (`internal/knowledge`) re-rendered into the context every turn. This replaced AGK's continuation loop, which kept only the single most recent tool result, dropped the original user message after the first tool call, and nudged the model to stop calling tools — together starving the deep agent of working memory. See the `internal/inspect` and `internal/knowledge` notes below.

See `README.md` for the user-facing description and `plan.txt` for the original design notes.

## Commands

```sh
go build ./...                       # build everything
go build -o testdiag .               # build the CLI binary
go vet ./...                         # vet
go test ./...                        # run all tests
go test ./internal/workspace/ -run TestResolve -v   # run a single test

# Run it (needs config + a reachable LLM endpoint; see Setup in README.md):
go run . https://jenkins.example.com/job/myapp/1234/
go run . --output ./reports <url>
```

## Architecture

The pipeline is sequential: fetch failures → run each failure through the stage state machine independently, one at a time → write a report.

- **`main.go`** — CLI parsing via `github.com/gilramir/argparse/v2` using the
  idiomatic `Function` callback style: `main()` builds the `ArgumentParser`, registers
  all flags and positionals, sets `Command.Function` to `run(*options)`, and calls
  `ap.ParseAndExit()`. `ParseAndExit` owns `-h`, parse errors, and function errors
  so nothing calls `os.Exit` directly. `run()` handles config load, resolving the LLM
  for each stage (`cfg.LLMForStage` / `cfg.LLMForStageOptional`), starting the
  per-stage LLM proxies (a `proxyManager` runs at most one proxy per distinct
  `(endpoint, advertised tool set)` and repoints each LLM's `BaseURL`) **for the
  tool-less stages only** — PLANINSPECTION and DEEPINSPECT are no longer fronted
  by the proxy (the `internal/inspect` engine talks to the model server directly),
  and `process()`, which runs each failure through the `pipeline` one at a time in order.
  LLMs for optional stages (HYPOTHESIZE, SUMMARIZE, LESSONS, and all feedback stages)
  fall back to the logparse LLM when not explicitly configured. Each failed test is fully
  independent (no shared agent state); sequential execution keeps output and
  `run_script` approval prompts coherent for the operator. The `--always-fails`
  flag builds a `failmode.Mode{AlwaysFails: true}` and threads it into
  `pipeline.New`; without it the default is a flaky test.

- **`internal/failmode`** — a tiny package describing whether the failure is
  flaky (intermittent — the default) or `AlwaysFails` (deterministic, every run).
  `Mode` exposes framing strings (`Description`, `CausePrior`, `ConditionGuidance`,
  `MechanismLabel`, `FeedbackConditionCriterion`, `ShortLabel`) that LOGPARSE,
  HYPOTHESIZE, and DEEPINSPECT (plus their feedback gates) inject into their
  prompts, so a flaky run is steered toward nondeterminism (races/timing) while an
  `--always-fails` run is steered toward deterministic defects and regressions.

- **`internal/config`** — Two-level TOML config, then `TESTDIAG_*` env vars (env
  always wins). `Load()` bootstraps the workspace root before reading any file:
  `TESTDIAG_WORKSPACE_ROOT` if set, otherwise the nearest ancestor of CWD that
  contains a `.git` entry (walking up), otherwise CWD. It then reads
  `<workspace>/testdiag.toml` (project config, checked in) followed by
  `~/.config/testdiag/config.toml` (user overrides, API keys); both files accept
  every config key and later values override earlier ones. The user config path is
  returned by `UserConfigPath()` (renamed from `Path()` in the two-file redesign).
  LLMs are defined once under `[llms.<name>]` and each stage points at one by name
  under `[stages]`; `LLMForStage` resolves the pair and errors clearly if a required
  stage is unassigned. Optional stage assignments (hypothesize, summarize, lessons,
  all feedback stages) are resolved via `LLMForStageOptional` and fall back to a
  sensible default at the call site. Per-stage tuning knobs live under `[stage_config]`
  as a flat struct (`StageConfig`): `logparse_max_feedbacks`, `hypothesize_max_feedbacks`,
  `planinspection_max_feedbacks`, `planinspection_max_tool_iterations`,
  `deepinspect_max_feedbacks`, `deepinspect_max_tool_iterations`,
  `summarize_max_feedbacks`, and `inspect_max_knowledge_chars` (the character cap
  on the `internal/knowledge` fact tree rendered into the PLANINSPECTION/DEEPINSPECT
  context each turn; default 24000, env `TESTDIAG_INSPECT_MAX_KNOWLEDGE_CHARS`) —
  each has a `TESTDIAG_<STAGE>_*` env var. LESSONS has no config knob (no feedback
  gate, no tool iteration limit).
  `Workspace.ArchitectureDoc` (config key `workspace.architecture_doc`, env
  `TESTDIAG_ARCHITECTURE_DOC`) is the workspace-relative path to an architecture
  document HYPOTHESIZE reads.

- **`internal/pipeline`** — the stage state machine. `Pipeline.Run` threads a
  per-test `Context` through ordered `Stage`s, stopping at the first unrecoverable
  error, and returns a `FinalResult`. The stages are:
  - `download.go` — saves the combined log under `.testdiag/logs/`
  - `logparse.go` — tool-less agent excerpts the log and writes the brief to
    `.testdiag/handoff/<test>.logparse.md`; retries with FEEDBACK critique up to
    `maxFeedbacks` times
  - `hypothesize.go` — tool-less agent reads the brief + arch doc and produces a
    numbered hypothesis list (`.testdiag/handoff/<test>.hypothesize.md`); parses
    `## Hypothesis N: title` headers via regex; retries with FEEDBACK up to
    `maxFeedbacks` times; errors if 0 hypotheses result
  - `deepinspect.go` — `deepInspectAllStage` iterates over `sc.Hypotheses`, calls
    `diagnose.Diagnoser.Diagnose` per hypothesis, runs FEEDBACK on each result, and
    soft-fails any hypothesis whose agent errored or whose feedback was exhausted;
    results accumulate in `sc.DeepInspects`; calls `tools.ResetToolLog()` before each
    hypothesis and `tools.CollectToolLog()` after, writing the compact tool-call log to
    `.testdiag/handoff/<test>.h<N>.deepinspect.tools.md` and a JSON dump of the run's
    fact tree to `.testdiag/handoff/<test>.h<N>.deepinspect.knowledge.json`
  - `planinspect.go` — `planInspectAllStage`, the PLANINSPECTION analogue of the
    above (same tool-log + `.planinspect.knowledge.json` dumps). After each plan it
    runs a **deterministic Go gate**: `missingPlanFiles` parses the leading
    backtick-quoted path from each list entry, resolves it through the workspace,
    and forces a FEEDBACK revision listing any path that does not exist — the LLM
    feedback gate cannot reliably verify file existence, so Go does it directly
  - `summarize.go` — tool-less agent that produces two things: (1) for each
    hypothesis, a short paragraph — if a DEEPINSPECT result exists it explains
    what was found (confirmed/refuted/inconclusive) and the key evidence; if the
    inspection failed or was not run it says so explicitly rather than skipping
    or speculating; (2) a "Most Likely Root Cause" section that names the
    best-supported hypothesis (or states that none is well-supported). Output is
    written to `.testdiag/handoff/<test>.summarize.md`; retries with FEEDBACK
    critique up to `maxFeedbacks` times. The FEEDBACK gate checks that every
    hypothesis has a section and that the most-likely verdict is present.
  - `lessons.go` — `lessonsStage` is the final stage; no tools, no feedback gate.
    `gatherHandoffs` globs `.testdiag/handoff/<test>.*.md` (picking up all prose
    handoffs and both `.(plan|deep)inspect.tools.md` tool logs automatically).
    `buildLessonsPrompt` injects the arch doc + sorted handoff files into the user
    message. The system prompt asks the LLM to evaluate prompt quality, tool usage
    efficiency, stage design, and what worked well, and to produce actionable
    developer suggestions. Output: `.testdiag/handoff/<test>.lessons.md` (set in
    `sc.LessonsPath`).
  - `feedback.go` — `feedbackChecker` is a shared struct with a configurable
    `systemPrompt` field; each stage gate uses a different prompt constant
    (`logParseFeedbackPrompt`, `hypothesizeFeedbackPrompt`, `planInspectFeedbackPrompt`,
    `deepInspectFeedbackPrompt`, `summarizeFeedbackPrompt`). `feedbackChecker.Check`
    returns APPROVED or a critique string the caller uses to build a retry prompt.

  Key types in `pipeline.go`: `Hypothesis{Index, Title, Description}`,
  `PlanInspectOutcome{Hypothesis, Content, ToolsCalled, FeedbackApproved, Failed,
  FailReason}`, `DeepInspectOutcome{Hypothesis, Content, ToolsCalled, FeedbackApproved,
  Failed, FailReason}`, `FinalResult{…, Summary, LessonsPath, …}` (returned by `Run`,
  consumed by `report`), `StageSpec{LLM, FeedbackLLM, ResetCounter}`,
  `PipelineSpec{LogParse, Hypothesize, Plan, DeepInspect, Summarize, Lessons}`
  (passed to `New`).

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
  glob/substring locate), history (`git_blame` line-range blame, `git_log` recent
  commits with optional `--patch` — both shell out to `git` with the workspace
  root as CWD and are output-capped; used to spot recent regressions), and
  log-oriented (`read_log` with `tail`, `grep_log` with context lines). They implement `v1beta.Tool` with a `JSONSchema()` so the provider
  calls them natively. `Register(ws)` registers them **once at startup** via the global
  `vnext.RegisterInternalTool` before any agent is built, so each tool is a
  single shared, **stateless** instance reused across every test — never store
  per-test state on a tool. All have hard output caps (file size, line span,
  match/entry/file counts) to protect the context window. The two log tools
  (`read_log`, `grep_log`, named in `tools.LogToolNames`) are gated by a
  package-level flag: DEEPINSPECT calls `tools.SetLogToolsEnabled(false)` so they
  refuse to run, and they are also excluded from the tool set it advertises
  (`tools.SchemasExcluding(tools.LogToolNames...)`) — defense in depth so the deep
  agent works from the brief, not the raw log. One exception to the read-only rule:
  `run_script` writes and executes a shell/Python script in the workspace root, but
  only after the operator approves the exact script at a `1 = Yes / 2 = No` prompt;
  a decline runs nothing. The other writer is `notebook` (`append`/`read`): a
  per-hypothesis Markdown scratchpad. The `notebook` is still registered and
  unit-tested but is now **dormant** — the `internal/inspect` engine excludes it from
  the advertised tool set because the `internal/knowledge` fact tree is the working
  memory now, so nothing sets its path or calls it in production.
  `tools.Execute(ctx, name, args)` and `tools.Has(name)` let a caller that drives its
  own tool loop (the inspect engine) run a registered tool by name and still get
  loop-guarding, verbose logging, and the tool-call log; `tools.VerboseEnabled()` /
  `tools.DebugEnabled()` expose the process-global `-v`/`--debug` flags so the engine
  can mirror them. Every call goes through a `loggingTool`
  wrapper that also guards against loops: if the model makes the exact same
  `(tool, args)` call `loopThreshold` times in one run, the call is intercepted and
  replaced with a nudge to try a different approach. `diagnose`/`planner` call
  `tools.ResetLoopGuard()` before each run to scope detection to a single
  attempt. Tools that opt out via the `loopExempt` marker (the `notebook`) are
  never guarded.
  `toollog.go` implements a **tool-call log** (global process state with
  `ResetToolLog()` / `CollectToolLog()` / `appendToolCall()`): the `loggingTool`
  wrapper calls `appendToolCall` after each real tool execution (not loop-nudges),
  recording name, args, and a compact `summarizeValue` of the response (short strings
  quoted; long strings as `(N chars, M lines)`; slices as `N items`; maps recursed).
  `FormatToolLog` serializes to numbered Markdown sections. The pipeline stages
  (`planinspect.go`, `deepinspect.go`) call `ResetToolLog()` before each hypothesis
  run and `CollectToolLog()` after, writing the result to the handoff directory.

- **`internal/inspect`** — the **own tool-loop engine** that drives PLANINSPECTION
  and DEEPINSPECT (replacing AgenticGoKit for those stages). `NewEngine(llm, Options{
  MaxIterations, MaxChars, Schemas, Interrupt})` builds an engine; `Run(ctx,
  RunInput{System, Task})` drives the loop. Each turn sends exactly **two messages**
  — a static `System` prompt and a `user` message that is the freshly-rendered fact
  tree (`internal/knowledge`) plus a next-step instruction — then parses the reply
  with `toolproto.Parse`, executes any tool calls via `tools.Execute`, folds the
  results into the tree (`ingest.go`, with a generic fallback so no result is lost),
  and repeats. No tool call ⇒ the reply is the final answer; at `MaxIterations` it
  asks once more advertising no tools to force one. The fact tree is the working
  memory, so the engine does NOT keep a growing message array (this is the explicit
  fix for AGK's continuation loop, which kept only the latest tool result, dropped
  the original user message, and discouraged further tool calls). `client.go` is a
  small OpenAI `/chat/completions` client talking to the model server directly (no
  proxy); structured `tool_calls` are folded into the text via `toolproto.FromStructured`.
  Operator-interrupt support is reimplemented here via the `Interrupter` interface
  (satisfied by `*llmproxy.InterruptController`): a queued message becomes a sticky
  note re-injected into the system prompt every turn. Under `-v` the engine prints the
  fact tree before each round; under `--debug` it also prints the raw LLM response
  (it reads `tools.VerboseEnabled()`/`DebugEnabled()` since these stages bypass the
  proxy that used to dump the conversation).

- **`internal/knowledge`** — the **fact tree** the inspect engine accumulates. Two
  record kinds: per-file records storing a sparse `line→text` map so repeated or
  overlapping reads coalesce into non-overlapping intervals ("lines 10-30, 50-60"),
  plus explicit NOT-FOUND markers; and per-query records (search_repo/find_files/
  file_exists/git/grep) keyed by canonical `(tool, params)` so a repeated query is a
  no-op merge. `Render()` produces the Markdown the LLM sees; `JSON()` a debug dump.
  When the rendered Markdown exceeds the char budget (`inspect_max_knowledge_chars`),
  least-recently-referenced facts are evicted: file line-text is elided first
  (keeping the interval index), then whole records are dropped. Pure, well-tested.

- **`internal/diagnose`** — the DEEPINSPECT stage layer. `Diagnoser.New(ws, llm, mode,
  background, memory, maxToolIterations, maxChars, mapper, interrupt, drainFn)` creates
  a diagnoser. `Diagnose(ctx, DiagnoseInput{Test, Brief, Hypothesis, HypothesisIndex,
  Plan, PrevResult, Critique})` maps the test (via `internal/mapping`), hard-disables
  the log tools, builds a fresh `inspect.Engine` (advertising the source tools minus
  the log tools and the dormant `notebook`), and runs it once. When `PrevResult`+
  `Critique` are set, the `Task` includes the prior draft and feedback so the retry
  goes deeper. The `brief`, `hypothesis`, **inspection plan, and mapped source file**
  go in the **system prompt** so they persist across every turn of the loop. The
  prompt also (a) permits a serendipitous "## Alternative Cause Discovered" section,
  and (b) reframes the tool budget to verify both sides of the hypothesis boundary.
  No internal critique/revise loop — that is handled externally by
  `deepInspectAllStage` + `feedbackChecker`.

- **`internal/planner`** — the PLANINSPECTION stage layer; the survey analogue of
  `diagnose`. `New(ws, llm, background, memory, maxToolIterations, maxChars, mapper)`
  builds an `inspect.Engine` (same excluded tools, no interrupt) and `Plan(ctx,
  PlanInput)` runs it to produce the prioritized, annotated file list.

- **`internal/mapping`** — `MapTestToSource(mapperPath, workspaceRoot string, test)
  (Result, error)` runs the user-supplied mapper executable (`workspace.mapper` in
  config / `TESTDIAG_MAPPER` env) with `test.FullName()` as the sole argument and
  reads the source file path from its stdout. The subprocess runs with
  `workspaceRoot` as its CWD. When `mapperPath` is empty, or the mapper prints
  nothing, an empty `Result` is returned with no error. A non-zero exit returns an
  error, which `Diagnoser.Diagnose` treats as a soft warning — it logs to stderr
  and continues with an empty mapping so the agent can locate the file itself.

- **`internal/report`** — writes one Markdown root-cause report per test into the
  output dir. Takes `pipeline.FinalResult`; renders the SUMMARIZE output as the main
  body and appends a per-hypothesis DEEPINSPECT appendix (collapsed in `<details>`).

- **`internal/toolproto`** — normalizes the various native tool-call syntaxes
  open models emit (GPT-OSS Harmony, Gemma ` ```tool_code `, Mistral
  `[TOOL_CALLS]`, Nemotron `<TOOLCALL>`, Llama 3.x bare-JSON / `<|python_tag|>`,
  plus structured `tool_calls`) into the one text shape AgenticGoKit's parser
  recognizes: `TOOL_CALL{"name":...,"args":{...}}`. `Normalize` rewrites the text;
  `Parse` goes one step further and returns `[]Call` (normalizing first, then
  extracting every `TOOL_CALL{…}`) — that is the entry point the `internal/inspect`
  engine uses to drive its own loop. Pure functions; well covered by tests.

- **`internal/llmproxy`** — an in-process reverse proxy fronting the LLM endpoint.
  Needed because AgenticGoKit v0.5.x's OpenAI adapter does **no** native tool
  calling: it never sends a `tools` array and reads only `choices[].message.content`,
  leaving the agent to parse tool calls out of text. The proxy injects the workspace
  tools into each request and runs the response `content` (and any structured
  `tool_calls`) through `toolproto` before AgenticGoKit sees it. `main.go` starts it
  and repoints each LLM's `BaseURL` at it. It now fronts only the **tool-less** stages
  (LOGPARSE, HYPOTHESIZE, SUMMARIZE, LESSONS, and the FEEDBACK gates); PLANINSPECTION
  and DEEPINSPECT bypass it entirely because the `internal/inspect` engine does the
  proxy's two jobs (tool injection + tool-call normalization) itself. The
  `InterruptController` still lives here (it is the sole stdin reader, muxing
  `run_script` confirmations and operator-interrupt lines) and is passed to the
  inspect engine via the `inspect.Interrupter` interface.

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
- All agents are built with **no builder preset**: internal tools attach via
  `Tools.Enabled` alone (DiscoverInternalTools), and presets like `ChatAgent` would
  clobber the system prompt / temperature and re-enable memory after `WithConfig`.
- The `feedbackChecker` pattern (tool-less agent with a stage-specific system prompt
  that outputs `APPROVED` or `NEEDS REVISION: <critique>`) is the standard way to
  gate stage output quality. Each stage that needs a feedback gate constructs one
  in `pipeline.New` with the appropriate prompt constant from `feedback.go` and
  passes it to the stage constructor.
