package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// lessonsStage is the final stage in the pipeline. It reads every handoff file
// produced during the diagnosis (including the tool logs written by
// PLANINSPECTION and DEEPINSPECT), optionally the workspace architecture
// document, and asks an LLM to evaluate how testdiag performed and suggest
// concrete improvements to the program (better prompts, better tools, better
// stage design). The output is developer-facing meta-analysis, not a
// user-facing report.
type lessonsStage struct {
	ws          *workspace.Workspace
	llm         config.LLMSpec
	archDocPath string // workspace-relative; may be empty
	verbose     bool
	pauseFn     func()
}

func newLessonsStage(ws *workspace.Workspace, llm config.LLMSpec, archDocPath string, verbose bool, pauseFn func()) *lessonsStage {
	return &lessonsStage{ws: ws, llm: llm, archDocPath: archDocPath, verbose: verbose, pauseFn: pauseFn}
}

func (s *lessonsStage) Name() State { return StateLessons }

func (s *lessonsStage) Run(ctx context.Context, sc *Context) error {
	stageBanner(s.verbose, string(s.Name()), 1)
	handoffs, err := s.gatherHandoffs(sc.Test)
	if err != nil {
		return fmt.Errorf("gathering handoffs: %w", err)
	}
	archDoc := s.readArchDoc()

	agent, err := s.buildAgent(sc.Test)
	if err != nil {
		return fmt.Errorf("building agent: %w", err)
	}

	r, err := agent.Run(ctx, buildLessonsPrompt(sc.Test, archDoc, handoffs))
	if err != nil {
		return fmt.Errorf("agent run: %w", err)
	}
	content := strings.TrimSpace(r.Content)
	if content == "" {
		return fmt.Errorf("LESSONS agent returned empty output for %s", sc.Test.FullName())
	}

	return s.save(sc, content)
}

func (s *lessonsStage) save(sc *Context, content string) error {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rel := filepath.Join(handoffDir, sanitize(sc.Test.FullName())+".lessons.md")
	abs := filepath.Join(s.ws.Root(), rel)
	header := fmt.Sprintf("# Lessons (LESSONS): %s\n\n", sc.Test.FullName())
	if err := os.WriteFile(abs, []byte(header+strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		return err
	}
	sc.LessonsPath = filepath.ToSlash(rel)
	if s.verbose || s.pauseFn != nil {
		fmt.Fprintf(os.Stdout, "--- LESSONS output for %s ---\n%s\n--- end ---\n\n",
			sc.Test.FullName(), strings.TrimSpace(content))
	}
	if s.pauseFn != nil {
		s.pauseFn()
	}
	return nil
}

func (s *lessonsStage) buildAgent(test jenkins.FailedTest) (vnext.Agent, error) {
	name := "lessons-" + sanitize(test.FullName())
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: lessonsSystemPrompt,
			LLM: vnext.LLMConfig{
				Provider:    s.llm.Provider,
				Model:       s.llm.Model,
				BaseURL:     s.llm.BaseURL,
				APIKey:      s.llm.APIKey,
				Temperature: s.llm.Temperature,
				MaxTokens:   s.llm.MaxTokens,
			},
			Tools:   &vnext.ToolsConfig{Enabled: false},
			Memory:  &vnext.MemoryConfig{Enabled: false},
			Timeout: 10 * time.Minute,
		}).
		Build()
}

const lessonsSystemPrompt = `You are reviewing the performance of testdiag, an automated flaky-test diagnosis tool. testdiag runs a pipeline of LLM stages:

  DOWNLOAD → LOGPARSE → HYPOTHESIZE → PLANINSPECTION × N → DEEPINSPECT × N → SUMMARIZE → LESSONS

- LOGPARSE: reads the raw failure log and writes a structured investigation brief
- HYPOTHESIZE: produces 1–3 ranked hypotheses about the nondeterministic condition from the brief + architecture doc
- PLANINSPECTION: one tool-using agent per hypothesis; breadth-first workspace survey producing a prioritized file list for DEEPINSPECT
- DEEPINSPECT: one tool-using agent per hypothesis; inspects source files to confirm, refute, or leave inconclusive each hypothesis
- SUMMARIZE: summarizes each hypothesis's inspection result (or notes that none is available) and identifies the most likely root cause

You will be shown all the handoff files produced during one diagnosis run. These include the prose outputs of each stage AND tool logs for the tool-using stages (compact summaries of each tool call and response — not the full content).

Your goal is to help the developer improve testdiag as a program. Analyze the run and write concrete, actionable suggestions. Think about:
- **Prompt quality**: Were any stage outputs weak, confused, or missing important information? What prompt changes would help?
- **Tool usage**: Did the agents call tools efficiently? Were there redundant, missing, or poorly-scoped calls? Would different tool designs help?
- **Stage design**: Was the pipeline well-suited to this problem? Are there missing stages, redundant stages, or handoff content that should be restructured?
- **What worked well**: Note anything that functioned effectively and should be preserved.

Be specific — reference actual stage outputs, tool call patterns, or response quality to justify each suggestion. Distinguish between issues specific to this particular run and systematic issues worth fixing in the program.

Output Markdown with clear section headers.`

// buildLessonsPrompt assembles the user message for the LESSONS agent.
func buildLessonsPrompt(test jenkins.FailedTest, archDoc string, handoffs map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Below are all the artifacts from the testdiag diagnosis of **%s**.\n\n", test.FullName())

	if strings.TrimSpace(archDoc) != "" {
		b.WriteString("## Architecture document (for context)\n\n")
		b.WriteString(strings.TrimSpace(archDoc))
		b.WriteString("\n\n")
	}

	b.WriteString("## Pipeline artifacts\n\n")
	keys := make([]string, 0, len(handoffs))
	for k := range handoffs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", name, strings.TrimSpace(handoffs[name]))
	}

	b.WriteString("Analyze how testdiag performed on this run and suggest concrete improvements to the program.")
	return b.String()
}

// gatherHandoffs finds all <sanitized-test>.*.md files in the handoff
// directory (including tool logs) and returns a filename→content map.
func (s *lessonsStage) gatherHandoffs(test jenkins.FailedTest) (map[string]string, error) {
	dir := filepath.Join(s.ws.Root(), handoffDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := sanitize(test.FullName()) + "."
	result := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		result[name] = string(data)
	}
	return result, nil
}

func (s *lessonsStage) readArchDoc() string {
	if s.archDocPath == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.ws.Root(), s.archDocPath))
	if err != nil {
		return ""
	}
	return string(data)
}
