package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/workspace"
)

// Defaults for the log-oriented tools.
const (
	defaultLogContext = 3   // grep_log context lines on each side of a match
	maxLogContext     = 20  // hard cap on grep_log context
	logExcerptHalf    = 200 // head/tail size when read_log returns a whole-log excerpt
)

// ---------------------------------------------------------------------------
// read_log
// ---------------------------------------------------------------------------

type readLogTool struct{ ws *workspace.Workspace }

func (t *readLogTool) Name() string { return "read_log" }
func (t *readLogTool) Description() string {
	return "Read the saved failure log (or any workspace file) with line numbers. With 'tail' it returns the last N lines, where a fatal error usually is. Otherwise it returns the whole file, eliding the middle of very large logs into a head+tail excerpt. Pass the log path you were given."
}
func (t *readLogTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string", "description": "Workspace-relative path to the log file (the path you were given for the failure log)."},
			"tail": map[string]interface{}{"type": "integer", "description": "If set, return only the last N lines instead of the whole log."},
		},
		"required": []string{"path"},
	}
}
func (t *readLogTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	path, has := strArg(args, "path")
	if !has {
		return fail("read_log: 'path' is required")
	}
	abs, err := t.ws.Resolve(path)
	if err != nil {
		return fail("read_log: %v", err)
	}
	data, fileTrunc, err := readCapped(abs)
	if err != nil {
		return fail("read_log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	total := len(lines)

	var b strings.Builder
	truncated := fileTrunc
	if tail, ok := intArg(args, "tail"); ok && tail > 0 {
		if tail > maxLineSpan {
			tail = maxLineSpan
		}
		from := total - tail
		if from < 0 {
			from = 0
		}
		if from > 0 {
			truncated = true
		}
		numberLines(&b, lines, from, total)
	} else if total > 2*logExcerptHalf {
		truncated = true
		numberLines(&b, lines, 0, logExcerptHalf)
		fmt.Fprintf(&b, "... [%d lines omitted — use 'tail' or grep_log to see more] ...\n", total-2*logExcerptHalf)
		numberLines(&b, lines, total-logExcerptHalf, total)
	} else {
		numberLines(&b, lines, 0, total)
	}

	return ok(map[string]interface{}{
		"path":      t.ws.Rel(abs),
		"lines":     total,
		"truncated": truncated,
		"text":      b.String(),
	}), nil
}

// ---------------------------------------------------------------------------
// grep_log
// ---------------------------------------------------------------------------

type grepLogTool struct{ ws *workspace.Workspace }

func (t *grepLogTool) Name() string { return "grep_log" }
func (t *grepLogTool) Description() string {
	return "Search a log (or any workspace file) for a regular-expression pattern and return matching lines WITH surrounding context lines, so you can read the stack frames around an error (e.g. 'Caused by', 'Traceback', 'FATAL'). Matching lines are marked with '>'. Like grep but context-aware — ideal for the failure log."
}
func (t *grepLogTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":        map[string]interface{}{"type": "string", "description": "Workspace-relative path to the file to search."},
			"pattern":     map[string]interface{}{"type": "string", "description": "RE2 regular expression to match against each line."},
			"context":     map[string]interface{}{"type": "integer", "description": fmt.Sprintf("Lines of context to show on each side of a match (default %d, max %d).", defaultLogContext, maxLogContext)},
			"ignore_case": map[string]interface{}{"type": "boolean", "description": "Case-insensitive match (default false)."},
		},
		"required": []string{"path", "pattern"},
	}
}
func (t *grepLogTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	path, has := strArg(args, "path")
	if !has {
		return fail("grep_log: 'path' is required")
	}
	pattern, hasPat := strArg(args, "pattern")
	if !hasPat {
		return fail("grep_log: 'pattern' is required")
	}
	expr := pattern
	if boolArg(args, "ignore_case") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return fail("grep_log: invalid pattern: %v", err)
	}
	ctxLines := defaultLogContext
	if v, ok := intArg(args, "context"); ok && v >= 0 {
		ctxLines = v
	}
	if ctxLines > maxLogContext {
		ctxLines = maxLogContext
	}

	abs, err := t.ws.Resolve(path)
	if err != nil {
		return fail("grep_log: %v", err)
	}
	data, _, err := readCapped(abs)
	if err != nil {
		return fail("grep_log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	total := len(lines)

	// Collect matched line numbers (1-based), then expand each into a context
	// window and emit the union, marking the matched lines and separating
	// non-contiguous blocks with "--".
	isMatch := make(map[int]bool)
	show := make(map[int]bool)
	matchCount := 0
	truncated := false
	for n := 1; n <= total; n++ {
		if !re.MatchString(lines[n-1]) {
			continue
		}
		if matchCount >= maxGrepMatches {
			truncated = true
			break
		}
		matchCount++
		isMatch[n] = true
		for j := n - ctxLines; j <= n+ctxLines; j++ {
			if j >= 1 && j <= total {
				show[j] = true
			}
		}
	}

	var b strings.Builder
	prev := 0
	for n := 1; n <= total; n++ {
		if !show[n] {
			continue
		}
		if prev != 0 && n != prev+1 {
			b.WriteString("--\n")
		}
		sep := ":"
		if isMatch[n] {
			sep = ">"
		}
		fmt.Fprintf(&b, "%d%s %s\n", n, sep, strings.TrimRight(lines[n-1], "\r"))
		prev = n
	}

	return ok(map[string]interface{}{
		"path":      t.ws.Rel(abs),
		"count":     matchCount,
		"truncated": truncated,
		"text":      b.String(),
	}), nil
}

// numberLines writes lines[from:to] (0-based, half-open) to b, prefixed with
// their 1-based line numbers.
func numberLines(b *strings.Builder, lines []string, from, to int) {
	for i := from; i < to; i++ {
		fmt.Fprintf(b, "%d: %s\n", i+1, strings.TrimRight(lines[i], "\r"))
	}
}
