// Package distill implements the post-test memorization step. After each test
// is fully diagnosed it reads all the pipeline handoff files and runs a
// tool-less LLM pass to extract durable, reusable codebase facts. Those facts
// are appended to .testdiag/memory.md so future runs can read them.
package distill

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

const (
	handoffDir = ".testdiag/handoff"
	memoryFile = ".testdiag/memory.md"
)

// systemPrompt instructs the distillation LLM.
const systemPrompt = `You are a codebase knowledge extractor. You will be shown all the investigation handoff files from one completed test diagnosis — the log analysis brief, hypotheses, inspection plans, deep-inspection results, and combined root cause analysis.

Extract ONLY facts that are:
- Specific to THIS codebase (file paths, function names, module roles, shared resources, configuration patterns)
- Durable (true across test runs, not specific to this single failure's circumstances)
- Reusable (would help a future agent locate relevant code faster in a future investigation)

Do NOT extract:
- The specific failure details, verdict, or conclusion for this run
- Vague generalizations ("the code handles X")
- Facts already obvious from directory names or file extensions

Output ONLY a Markdown bullet list (using - bullets). One fact per bullet. No preamble, no section headers, no trailing text. If there is nothing worth saving, output only the single word NONE.

Good examples:
- ` + "`src/server/tcp_handler.cc`" + ` is the entry point for all TCP connection setup; timeout is in ` + "`config/timeouts.toml`" + `
- Test helper ` + "`test_utils.h:makeTestClient()`" + ` creates a real TCP socket, not a mock
- Global singleton ` + "`ResourcePool`" + ` in ` + "`src/pool/resource_pool.h`" + ` is shared across all test threads

Bad examples (too vague, or failure-specific):
- The test failed due to a race condition
- There are source files in the src/ directory`

// Distiller extracts durable codebase facts from pipeline handoff files and
// appends them to the shared memory file after each test is diagnosed.
type Distiller struct {
	ws  *workspace.Workspace
	llm config.LLMSpec
}

// New creates a Distiller.
func New(ws *workspace.Workspace, llm config.LLMSpec) *Distiller {
	return &Distiller{ws: ws, llm: llm}
}

// Distill reads all handoff files for the given test, runs the extraction LLM,
// and appends any discovered facts to .testdiag/memory.md. Errors are
// non-fatal; the caller should log them but continue.
func (d *Distiller) Distill(ctx context.Context, test jenkins.FailedTest) error {
	handoffs, err := d.gatherHandoffs(test)
	if err != nil {
		return fmt.Errorf("gathering handoffs: %w", err)
	}
	if len(handoffs) == 0 {
		return nil
	}

	userMsg := buildPrompt(test, handoffs)

	name := "distill-" + sanitize(test.FullName())
	agent, err := vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: systemPrompt,
			LLM: vnext.LLMConfig{
				Provider:    d.llm.Provider,
				Model:       d.llm.Model,
				BaseURL:     d.llm.BaseURL,
				APIKey:      d.llm.APIKey,
				Temperature: d.llm.Temperature,
				MaxTokens:   d.llm.MaxTokens,
			},
			Tools:   &vnext.ToolsConfig{Enabled: false},
			Memory:  &vnext.MemoryConfig{Enabled: false},
			Timeout: 5 * time.Minute,
		}).
		Build()
	if err != nil {
		return fmt.Errorf("building distill agent: %w", err)
	}

	r, err := agent.Run(ctx, userMsg)
	if err != nil {
		return fmt.Errorf("distill agent run: %w", err)
	}

	facts := strings.TrimSpace(r.Content)
	if facts == "" || strings.EqualFold(facts, "NONE") {
		return nil
	}

	return d.appendMemory(test, facts)
}

// gatherHandoffs finds all <sanitized-test>.*.md files in the handoff directory
// and returns a map from filename to content, sorted by filename.
func (d *Distiller) gatherHandoffs(test jenkins.FailedTest) (map[string]string, error) {
	dir := filepath.Join(d.ws.Root(), handoffDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	prefix := sanitize(test.FullName()) + "."
	result := make(map[string]string)
	var names []string
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
		names = append(names, name)
	}

	// Keep only files that matched; sort for deterministic prompt order.
	sort.Strings(names)
	ordered := make(map[string]string, len(names))
	for _, n := range names {
		ordered[n] = result[n]
	}
	return ordered, nil
}

// appendMemory appends extracted facts to .testdiag/memory.md.
func (d *Distiller) appendMemory(test jenkins.FailedTest, facts string) error {
	path := filepath.Join(d.ws.Root(), memoryFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	header := fmt.Sprintf("\n<!-- %s — %s -->\n", test.FullName(), timestamp)
	_, err = fmt.Fprintf(f, "%s%s\n", header, facts)
	return err
}

// buildPrompt assembles the distillation user message.
func buildPrompt(test jenkins.FailedTest, handoffs map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The test **%s** has been investigated. Below are all the pipeline handoff files.\n\n", test.FullName())

	// Sort keys for deterministic output.
	keys := make([]string, 0, len(handoffs))
	for k := range handoffs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, name := range keys {
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", name, strings.TrimSpace(handoffs[name]))
	}

	b.WriteString("Extract durable, reusable codebase facts as described. Output only the bullet list, or NONE.")
	return b.String()
}

func sanitize(s string) string {
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}
	out := strings.Map(repl, s)
	if len(out) > 180 {
		out = out[:180]
	}
	if out == "" {
		return "test"
	}
	return out
}
