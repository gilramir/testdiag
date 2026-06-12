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
3. For each failure, in parallel:
   - Maps the test to its source file (project-specific — **stubbed**).
   - Saves the full failure log into the workspace so the file tools can read it.
   - Builds a fresh AgenticGoKit agent and runs the **provider's native
     tool-calling loop**: the LLM reads the log, then navigates the source with
     the tools below until it can explain the failure.
4. Writes one Markdown root-cause report per test under `test-diagnosis/`.

Each test is diagnosed independently (its own agent, no shared memory), so they
parallelize cleanly.

## Tools given to the LLM

All are **jailed to the workspace root** — the model cannot read outside the
checkout. They are AgenticGoKit *internal tools* exposing JSON Schemas, so the
provider can call them natively:

| Tool | Purpose |
|------|---------|
| `read_file` | Read an entire (small) file |
| `list_directory` | List a directory's entries |
| `count_lines` | `wc -l` for one or more files |
| `read_lines` | Read a single line or an inclusive range |
| `grep` | Find matching lines (with numbers) in a file |

The prompt steers the model to `count_lines`/`grep`/`read_lines` rather than
dumping whole files, so large logs and large sources stay within context.

## Setup

```sh
go mod tidy                      # download AgenticGoKit + deps
cp config.example.toml ~/.config/testdiag/config.toml
$EDITOR ~/.config/testdiag/config.toml
```

Configuration (file + `TESTDIAG_*` env overrides) is documented in
[`config.example.toml`](config.example.toml). At minimum set the LLM
`base_url` + `model` and your Jenkins `user` + `api_token`.

Put a `TEST_AGENT.md` at the root of the workspace you run against; its contents
are injected into every diagnosis as background context.

## Usage

```sh
# Run from inside the build's checkout (or set workspace.root in config):
testdiag https://jenkins.example.com/job/myapp/1234/

# Overrides:
testdiag -j 8 --output ./reports https://jenkins.example.com/job/myapp/1234/testReport/

# Filter to a subset of failures: pass one or more substrings after the URL.
# Only failed tests whose name (class.method) contains any of them are
# diagnosed; with no substrings, every failed test is processed.
testdiag https://jenkins.example.com/job/myapp/1234/ 100 LoginTest
```

## Placeholders

These are intentionally left for you to complete:

- **Test → source-file mapping** — `internal/mapping/mapping.go`
  (`MapTestToSource`). This is project-specific (language, repo layout,
  package→path rules). It currently returns an empty path, which is safe: the
  agent will locate the file itself via `list_directory`/`grep`. Implement it to
  give the agent a precise starting point.

## Layout

```
main.go                     CLI + parallel orchestration
internal/config             config file + env overrides
internal/jenkins            fetch /api/json, parse failed cases
internal/mapping            test -> source file  (STUB)
internal/workspace          path jail for the file tools
internal/tools              the 5 file tools (native-schema internal tools)
internal/diagnose           per-test agent build, prompt, tool loop
internal/report             Markdown report writer
```

The `AgenticGoKit/` directory in this tree is a local clone for reference only;
it is git-ignored and not used by the build (the dependency is fetched normally
via `go.mod`).
