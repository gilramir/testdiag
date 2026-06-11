package tools

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/workspace"
)

// Caps for the tree-walking tools. They protect both the context window and the
// wall-clock cost of crawling a large checkout.
const (
	maxRepoMatches  = 200   // max matches returned by search_repo
	maxFindResults  = 500   // max paths returned by find_files
	maxFilesScanned = 20000 // max files visited by a single walk
)

// skipDirs are directory names never descended into by search_repo / find_files.
// They are either VCS metadata, dependency caches, build output, or our own log
// directory — none of which is the project source the diagnosis cares about.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true,
	"__pycache__": true, ".mypy_cache": true, ".pytest_cache": true,
	".testdiag": true,
	"build":     true, "dist": true, ".tox": true, ".venv": true,
}

// ---------------------------------------------------------------------------
// search_repo (recursive grep across the workspace tree)
// ---------------------------------------------------------------------------

type searchRepoTool struct{ ws *workspace.Workspace }

func (t *searchRepoTool) Name() string { return "search_repo" }
func (t *searchRepoTool) Description() string {
	return "Recursively search the workspace tree for a regular-expression pattern and return matching lines as path:line: text. Use this to locate a symbol, an error string from the log, or a test's source file across the whole project. Skips VCS, dependency, and build directories and binary files."
}
func (t *searchRepoTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern":     map[string]interface{}{"type": "string", "description": "RE2 regular expression to match against each line."},
			"path":        map[string]interface{}{"type": "string", "description": "Workspace-relative directory to limit the search to. Defaults to the whole workspace ('.')."},
			"include":     map[string]interface{}{"type": "string", "description": "Optional filename glob to restrict which files are searched (e.g. '*.py' or '*Test.java')."},
			"ignore_case": map[string]interface{}{"type": "boolean", "description": "Case-insensitive match (default false)."},
		},
		"required": []string{"pattern"},
	}
}
func (t *searchRepoTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	pattern, has := strArg(args, "pattern")
	if !has {
		return fail("search_repo: 'pattern' is required")
	}
	expr := pattern
	if boolArg(args, "ignore_case") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return fail("search_repo: invalid pattern: %v", err)
	}
	base := "."
	if p, ok := strArg(args, "path"); ok {
		base = p
	}
	root, err := t.ws.Resolve(base)
	if err != nil {
		return fail("search_repo: %v", err)
	}
	include, hasInclude := strArg(args, "include")

	var matches []map[string]interface{}
	filesScanned := 0
	truncated := false

	walkErr := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting the walk
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if abs != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if hasInclude {
			if m, _ := filepath.Match(include, d.Name()); !m {
				return nil
			}
		}
		if filesScanned >= maxFilesScanned {
			truncated = true
			return filepath.SkipAll
		}
		filesScanned++

		data, _, rerr := readCapped(abs)
		if rerr != nil || isBinary(data) {
			return nil
		}
		rel := t.ws.Rel(abs)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				if len(matches) >= maxRepoMatches {
					truncated = true
					return filepath.SkipAll
				}
				matches = append(matches, map[string]interface{}{
					"path": filepath.ToSlash(rel),
					"line": i + 1,
					"text": strings.TrimRight(line, "\r"),
				})
			}
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return fail("search_repo: %v", ctx.Err())
	}

	return ok(map[string]interface{}{
		"matches":   matches,
		"count":     len(matches),
		"truncated": truncated,
	}), nil
}

// ---------------------------------------------------------------------------
// find_files (locate files by name / glob across the tree)
// ---------------------------------------------------------------------------

type findFilesTool struct{ ws *workspace.Workspace }

func (t *findFilesTool) Name() string { return "find_files" }
func (t *findFilesTool) Description() string {
	return "Find files in the workspace whose name matches a glob (e.g. '*Test.java', 'foo_client.py') or whose path contains a substring. Returns workspace-relative paths. Use this to locate a test's source file instead of crawling directories by hand."
}
func (t *findFilesTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern":     map[string]interface{}{"type": "string", "description": "Filename glob (e.g. '*Test.java') matched against each file's name, or a plain substring matched against the full path."},
			"path":        map[string]interface{}{"type": "string", "description": "Workspace-relative directory to limit the search to. Defaults to the whole workspace ('.')."},
			"ignore_case": map[string]interface{}{"type": "boolean", "description": "Case-insensitive match (default false)."},
		},
		"required": []string{"pattern"},
	}
}
func (t *findFilesTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	pattern, has := strArg(args, "pattern")
	if !has {
		return fail("find_files: 'pattern' is required")
	}
	base := "."
	if p, ok := strArg(args, "path"); ok {
		base = p
	}
	root, err := t.ws.Resolve(base)
	if err != nil {
		return fail("find_files: %v", err)
	}
	ci := boolArg(args, "ignore_case")

	var paths []string
	filesScanned := 0
	truncated := false

	walkErr := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if abs != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if filesScanned >= maxFilesScanned {
			truncated = true
			return filepath.SkipAll
		}
		filesScanned++
		rel := filepath.ToSlash(t.ws.Rel(abs))
		if matchPath(pattern, rel, d.Name(), ci) {
			if len(paths) >= maxFindResults {
				truncated = true
				return filepath.SkipAll
			}
			paths = append(paths, rel)
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return fail("find_files: %v", ctx.Err())
	}
	sort.Strings(paths)

	return ok(map[string]interface{}{
		"paths":     paths,
		"count":     len(paths),
		"truncated": truncated,
	}), nil
}

// matchPath reports whether a file matches the find_files pattern. A pattern
// containing glob metacharacters is matched (via filepath.Match) against both
// the base name and the relative path, tolerating a leading "**/"; a plain
// pattern is matched as a case-optional substring of the path.
func matchPath(pattern, rel, base string, ci bool) bool {
	p := strings.TrimPrefix(pattern, "**/")
	if strings.ContainsAny(pattern, "*?[") {
		if m, _ := filepath.Match(p, base); m {
			return true
		}
		if m, _ := filepath.Match(pattern, rel); m {
			return true
		}
		if m, _ := filepath.Match(p, rel); m {
			return true
		}
		return false
	}
	needle, hay := pattern, rel
	if ci {
		needle, hay = strings.ToLower(pattern), strings.ToLower(rel)
	}
	return strings.Contains(hay, needle)
}

// isBinary reports whether data looks like a binary file (contains a NUL byte in
// the leading window), so the text tools can skip it.
func isBinary(data []byte) bool {
	const window = 8000
	if len(data) > window {
		data = data[:window]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// readCapped reads up to maxFileBytes of a file, reporting whether it was
// truncated. Used by the tree walkers and the log tools.
func readCapped(abs string) (data []byte, truncated bool, err error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	buf := make([]byte, maxFileBytes+1)
	n, _ := readFull(f, buf)
	if n > maxFileBytes {
		return buf[:maxFileBytes], true, nil
	}
	return buf[:n], false, nil
}
