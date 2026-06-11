// Package tools implements the workspace file-inspection tools exposed to the
// LLM as AgenticGoKit internal tools. Each tool implements
// v1beta.ToolWithSchema (Name/Description/Execute + JSONSchema) so the agent
// can call them via the provider's native tool-calling loop.
//
// Every tool resolves its paths through a single shared *workspace.Workspace,
// so the model can only read files inside the build's checkout.
package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/workspace"
)

// Guards against pathological inputs blowing up the context window / memory.
const (
	maxFileBytes   = 2 << 20 // 2 MiB for a whole-file read
	maxLineSpan    = 2000    // max lines returned by read_lines
	maxGrepMatches = 200     // max matches returned by grep
	maxDirEntries  = 1000    // max entries returned by list_directory
)

// toolDefs is the canonical list of workspace tools. Both Register (which wires
// them into the agent) and Schemas (which advertises them to the model) build
// from this single source so the two never drift.
func toolDefs(ws *workspace.Workspace) []vnext.Tool {
	return []vnext.Tool{
		&readFileTool{ws: ws},
		&listDirTool{ws: ws},
		&countLinesTool{ws: ws},
		&readLinesTool{ws: ws},
		&grepTool{ws: ws},
		&searchRepoTool{ws: ws},
		&findFilesTool{ws: ws},
		&gitBlameTool{ws: ws},
		&gitLogTool{ws: ws},
		&readLogTool{ws: ws},
		&grepLogTool{ws: ws},
	}
}

// Register registers all workspace tools against ws. Call once at startup,
// before building any agent. The returned slice is the tool names registered
// (useful for logging / prompt construction).
func Register(ws *workspace.Workspace) []string {
	defs := toolDefs(ws)
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		d := d // capture for the factory closure
		vnext.RegisterInternalTool(d.Name(), func() vnext.Tool { return d })
		names = append(names, d.Name())
	}
	return names
}

// Schema is a tool's name, description, and JSON-schema parameters — enough to
// advertise the tool to an OpenAI-compatible server as a `tools` entry.
type Schema struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
}

// Schemas returns the schema of every workspace tool. The normalizing proxy
// uses these to inject a `tools` array into outbound requests, because
// AgenticGoKit's OpenAI adapter does not send tool definitions itself. No path
// is resolved here, so a nil workspace is fine.
func Schemas() []Schema {
	defs := toolDefs(nil)
	out := make([]Schema, 0, len(defs))
	for _, d := range defs {
		ws, ok := d.(vnext.ToolWithSchema)
		if !ok {
			continue
		}
		out = append(out, Schema{
			Name:        d.Name(),
			Description: d.Description(),
			Parameters:  ws.JSONSchema(),
		})
	}
	return out
}

func ok(content interface{}) *vnext.ToolResult {
	return &vnext.ToolResult{Success: true, Content: content}
}

func fail(format string, args ...interface{}) (*vnext.ToolResult, error) {
	msg := fmt.Sprintf(format, args...)
	return &vnext.ToolResult{Success: false, Error: msg}, fmt.Errorf("%s", msg)
}

func strArg(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key].(string)
	return v, ok && v != ""
}

// intArg extracts an integer, tolerating JSON numbers (float64) and strings.
func intArg(args map[string]interface{}, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

func boolArg(args map[string]interface{}, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

type readFileTool struct{ ws *workspace.Workspace }

func (t *readFileTool) Name() string { return "read_file" }
func (t *readFileTool) Description() string {
	return "Read the entire contents of a single file in the workspace. Prefer read_lines or grep for large files; reading a whole file may be truncated if it exceeds 2 MiB."
}
func (t *readFileTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Workspace-relative path to the file to read.",
			},
		},
		"required": []string{"path"},
	}
}
func (t *readFileTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	path, hasPath := strArg(args, "path")
	if !hasPath {
		return fail("read_file: 'path' is required")
	}
	abs, err := t.ws.Resolve(path)
	if err != nil {
		return fail("read_file: %v", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail("read_file: cannot open %q: %v. Paths must be WORKSPACE-RELATIVE "+
			"(e.g. %q) — do not pass an absolute path or prefix it with the workspace root. "+
			"Retry with the path relative to the workspace root, or use list_directory/grep to locate it.",
			path, err, t.ws.Rel(abs))
	}
	if info.IsDir() {
		return fail("read_file: %q is a directory (use list_directory)", path)
	}
	f, err := os.Open(abs)
	if err != nil {
		return fail("read_file: %v", err)
	}
	defer f.Close()

	buf := make([]byte, maxFileBytes+1)
	n, _ := readFull(f, buf)
	truncated := n > maxFileBytes
	if truncated {
		n = maxFileBytes
	}
	return ok(map[string]interface{}{
		"path":      t.ws.Rel(abs),
		"truncated": truncated,
		"content":   string(buf[:n]),
	}), nil
}

// ---------------------------------------------------------------------------
// list_directory
// ---------------------------------------------------------------------------

type listDirTool struct{ ws *workspace.Workspace }

func (t *listDirTool) Name() string { return "list_directory" }
func (t *listDirTool) Description() string {
	return "List the entries (files and sub-directories) of a directory in the workspace. Directories are suffixed with '/'."
}
func (t *listDirTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Workspace-relative directory path. Use '.' for the workspace root.",
			},
		},
		"required": []string{"path"},
	}
}
func (t *listDirTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	path, hasPath := strArg(args, "path")
	if !hasPath {
		path = "."
	}
	abs, err := t.ws.Resolve(path)
	if err != nil {
		return fail("list_directory: %v", err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return fail("list_directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	truncated := false
	if len(names) > maxDirEntries {
		names = names[:maxDirEntries]
		truncated = true
	}
	return ok(map[string]interface{}{
		"path":      t.ws.Rel(abs),
		"entries":   names,
		"truncated": truncated,
	}), nil
}

// ---------------------------------------------------------------------------
// count_lines (wc -l for one or more files)
// ---------------------------------------------------------------------------

type countLinesTool struct{ ws *workspace.Workspace }

func (t *countLinesTool) Name() string { return "count_lines" }
func (t *countLinesTool) Description() string {
	return "Count the number of lines (like `wc -l`) in one or more workspace files. Useful for sizing a file before reading ranges of it."
}
func (t *countLinesTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"paths": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "One or more workspace-relative file paths.",
			},
		},
		"required": []string{"paths"},
	}
}
func (t *countLinesTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	paths, err := stringSlice(args, "paths")
	if err != nil {
		return fail("count_lines: %v", err)
	}
	results := make([]map[string]interface{}, 0, len(paths))
	for _, p := range paths {
		entry := map[string]interface{}{"path": p}
		abs, err := t.ws.Resolve(p)
		if err != nil {
			entry["error"] = err.Error()
			results = append(results, entry)
			continue
		}
		n, err := countFileLines(abs)
		if err != nil {
			entry["error"] = err.Error()
		} else {
			entry["lines"] = n
		}
		results = append(results, entry)
	}
	return ok(map[string]interface{}{"files": results}), nil
}

// ---------------------------------------------------------------------------
// read_lines (a single line or an inclusive range)
// ---------------------------------------------------------------------------

type readLinesTool struct{ ws *workspace.Workspace }

func (t *readLinesTool) Name() string { return "read_lines" }
func (t *readLinesTool) Description() string {
	return "Read a single line or an inclusive range of lines from one workspace file. Lines are 1-based. Omit 'end' to read just the 'start' line. Returned text is prefixed with line numbers."
}
func (t *readLinesTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":  map[string]interface{}{"type": "string", "description": "Workspace-relative file path."},
			"start": map[string]interface{}{"type": "integer", "description": "First line to read (1-based)."},
			"end":   map[string]interface{}{"type": "integer", "description": "Last line to read (inclusive). Defaults to 'start' for a single line."},
		},
		"required": []string{"path", "start"},
	}
}
func (t *readLinesTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	path, hasPath := strArg(args, "path")
	if !hasPath {
		return fail("read_lines: 'path' is required")
	}
	start, hasStart := intArg(args, "start")
	if !hasStart {
		return fail("read_lines: 'start' is required")
	}
	end, hasEnd := intArg(args, "end")
	if !hasEnd {
		end = start
	}
	if start < 1 {
		start = 1
	}
	if end < start {
		return fail("read_lines: 'end' (%d) must be >= 'start' (%d)", end, start)
	}
	if end-start+1 > maxLineSpan {
		end = start + maxLineSpan - 1
	}

	abs, err := t.ws.Resolve(path)
	if err != nil {
		return fail("read_lines: %v", err)
	}
	lines, lastLine, err := readLineRange(abs, start, end)
	if err != nil {
		return fail("read_lines: %v", err)
	}
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%d: %s\n", start+i, ln)
	}
	return ok(map[string]interface{}{
		"path":  t.ws.Rel(abs),
		"start": start,
		"end":   lastLine,
		"text":  b.String(),
	}), nil
}

// ---------------------------------------------------------------------------
// grep
// ---------------------------------------------------------------------------

type grepTool struct{ ws *workspace.Workspace }

func (t *grepTool) Name() string { return "grep" }
func (t *grepTool) Description() string {
	return "Search a single workspace file for a regular-expression pattern and return matching lines with their 1-based line numbers. Use this to locate symbols, errors, or definitions in large files."
}
func (t *grepTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":        map[string]interface{}{"type": "string", "description": "Workspace-relative file path to search."},
			"pattern":     map[string]interface{}{"type": "string", "description": "RE2 regular expression to match against each line."},
			"ignore_case": map[string]interface{}{"type": "boolean", "description": "Case-insensitive match (default false)."},
		},
		"required": []string{"path", "pattern"},
	}
}
func (t *grepTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	path, hasPath := strArg(args, "path")
	if !hasPath {
		return fail("grep: 'path' is required")
	}
	pattern, hasPat := strArg(args, "pattern")
	if !hasPat {
		return fail("grep: 'pattern' is required")
	}
	expr := pattern
	if boolArg(args, "ignore_case") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return fail("grep: invalid pattern: %v", err)
	}
	abs, err := t.ws.Resolve(path)
	if err != nil {
		return fail("grep: %v", err)
	}
	matches, truncated, err := grepFile(abs, re)
	if err != nil {
		return fail("grep: %v", err)
	}
	return ok(map[string]interface{}{
		"path":      t.ws.Rel(abs),
		"matches":   matches,
		"truncated": truncated,
		"count":     len(matches),
	}), nil
}

// ---------------------------------------------------------------------------
// shared file helpers
// ---------------------------------------------------------------------------

func stringSlice(args map[string]interface{}, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("'%s' is required", key)
	}
	switch v := raw.(type) {
	case []string:
		return v, nil
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("'%s' must be an array of strings", key)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		return []string{v}, nil // tolerate a single path passed as a bare string
	default:
		return nil, fmt.Errorf("'%s' must be an array of strings", key)
	}
}

func readFull(f *os.File, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func countFileLines(abs string) (int, error) {
	f, err := os.Open(abs)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		count++
	}
	return count, sc.Err()
}

func readLineRange(abs string, start, end int) (lines []string, lastLine int, err error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		if n < start {
			continue
		}
		if n > end {
			break
		}
		lines = append(lines, sc.Text())
		lastLine = n
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if len(lines) == 0 {
		return nil, 0, fmt.Errorf("no lines in range %d-%d (file has %d lines)", start, end, n)
	}
	return lines, lastLine, nil
}

func grepFile(abs string, re *regexp.Regexp) (matches []map[string]interface{}, truncated bool, err error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if re.MatchString(line) {
			if len(matches) >= maxGrepMatches {
				truncated = true
				break
			}
			matches = append(matches, map[string]interface{}{
				"line": n,
				"text": strings.TrimRight(line, "\r"),
			})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	return matches, truncated, nil
}
