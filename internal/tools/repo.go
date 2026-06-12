package tools

import (
	"bufio"
	"bytes"
	"context"
	"io"
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
// directory — none of which is the project source the diagnosis cares about. The
// report output directory is added at startup via ExcludeDir so a tree search
// never reads the tool's own generated reports back in.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true,
	"__pycache__": true, ".mypy_cache": true, ".pytest_cache": true,
	".testdiag": true,
	"build":     true, "dist": true, ".tox": true, ".venv": true,
}

// ExcludeDir adds a directory base name to the set that search_repo and
// find_files never descend into. Call once at startup, before any agent runs
// (the set is read concurrently by the walkers but not mutated after). Used to
// skip the generated report directory, whose name is configurable. Trivial names
// ("", ".", "..") are ignored.
func ExcludeDir(name string) {
	if name == "" || name == "." || name == ".." {
		return
	}
	skipDirs[name] = true
}

// ---------------------------------------------------------------------------
// search_repo (recursive grep across the workspace tree)
// ---------------------------------------------------------------------------

type searchRepoTool struct{ ws *workspace.Workspace }

func (t *searchRepoTool) Name() string { return "search_repo" }
func (t *searchRepoTool) Description() string {
	return "Recursively search the workspace tree for a regular-expression pattern and return matching lines as path:line: text. Use this to locate a symbol, an error string from the log, or a test's source file across the whole project. This crawls the WHOLE tree and can be slow, so prefer narrower lookups first: if you already know a symbol is imported, follow its import to the defining file and grep that file instead; and when you must search, pass an include glob (e.g. *.py) and search for the definition (e.g. 'def name'/'class name') rather than every use. Skips VCS, dependency, and build directories and binary files."
}
func (t *searchRepoTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern":     map[string]interface{}{"type": "string", "description": "RE2 regular expression to match against each line."},
			"path":        map[string]interface{}{"type": "string", "description": "Workspace-relative directory to limit the search to. Defaults to the whole workspace ('.')."},
			"include":     map[string]interface{}{"type": "string", "description": "Optional filename glob to restrict which files are searched (e.g. *.py or *Test.java)."},
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
	// When the raw log is withheld (DEEPINSPECT), refuse a search that is clearly
	// hunting for a log file (e.g. "failure.log", "log.txt", "*.log") rather than
	// source code: there is no failure log in the workspace — it was consumed by
	// the earlier stage and everything from it is in the investigation brief.
	if !logToolsEnabled.Load() {
		if q := logHuntQuery(args); q != "" {
			return fail("search_repo: %q looks like an attempt to find a log file. There is no failure log in the workspace — it was consumed by the earlier stage and everything relevant is already in the investigation brief. Do NOT search for logs; go straight to the SOURCE files the brief names.", q)
		}
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

		if searchFile(abs, t.ws.Rel(abs), re, &matches) {
			truncated = true
			return filepath.SkipAll
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

// logFileQueryRe matches a query whose ENTIRETY names a log file — e.g.
// "failure.log", "log.txt", "*.log", "logs/app.log" — as opposed to a content
// search that merely mentions ".log" (like the pattern "\.log\("), which is left
// alone. The whole-string anchors keep it from flagging legitimate source
// searches.
var logFileQueryRe = regexp.MustCompile(`(?i)^[\w./*?-]*?([\w*?-]*\.log|log\.txt)$`)

// logHuntQuery returns the first search_repo argument (the grep pattern, the
// include glob, or the path) that looks like an attempt to locate a log file, or
// "" if none does.
func logHuntQuery(args map[string]interface{}) string {
	for _, key := range []string{"pattern", "include", "path"} {
		if v, ok := strArg(args, key); ok && logFileQueryRe.MatchString(strings.TrimSpace(v)) {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// find_files (locate files by name / glob across the tree)
// ---------------------------------------------------------------------------

type findFilesTool struct{ ws *workspace.Workspace }

func (t *findFilesTool) Name() string { return "find_files" }
func (t *findFilesTool) Description() string {
	return "Find files in the workspace whose name matches a glob (e.g. *Test.java, foo_client.py) or whose path contains a substring. Returns workspace-relative paths. Use this to locate a test's source file instead of crawling directories by hand."
}
func (t *findFilesTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern":     map[string]interface{}{"type": "string", "description": "Filename glob (e.g. *Test.java) matched against each file's name, or a plain substring matched against the full path."},
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
	// When the raw log is withheld (DEEPINSPECT), refuse a lookup that is clearly
	// hunting for a log file (e.g. "failure.log", "log.txt", "*.log"): there is no
	// failure log in the workspace — it was consumed by the earlier stage and
	// everything from it is in the investigation brief.
	if !logToolsEnabled.Load() {
		if q := logHuntQuery(args); q != "" {
			return fail("find_files: %q looks like an attempt to find a log file. There is no failure log in the workspace — it was consumed by the earlier stage and everything relevant is already in the investigation brief. Do NOT look for logs; go straight to the SOURCE files the brief names.", q)
		}
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

// binarySniffLen is how many leading bytes searchFile inspects to decide a file
// is binary before scanning it line by line.
const binarySniffLen = 8000

// searchFile scans one file line by line for re, appending matches (capped at
// maxRepoMatches across the whole walk) to *matches. It STREAMS the file with a
// small buffer — reading at most maxFileBytes — rather than loading the whole
// file into memory, and skips files that look binary from their first bytes.
// Returns true when the global match cap is reached so the walk can stop.
//
// This is the hot path of search_repo: it runs once per regular file in the
// tree, so it must not allocate per-file buffers proportional to maxFileBytes.
func searchFile(abs, rel string, re *regexp.Regexp, matches *[]map[string]interface{}) (capped bool) {
	f, err := os.Open(abs)
	if err != nil {
		return false // unreadable: skip, like the rest of the walk
	}
	defer f.Close()

	r := bufio.NewReaderSize(io.LimitReader(f, maxFileBytes), 64*1024)
	if head, _ := r.Peek(binarySniffLen); isBinary(head) {
		return false
	}
	relSlash := filepath.ToSlash(rel)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if !re.MatchString(text) {
			continue
		}
		if len(*matches) >= maxRepoMatches {
			return true
		}
		*matches = append(*matches, map[string]interface{}{
			"path": relSlash,
			"line": line,
			"text": strings.TrimRight(text, "\r"),
		})
	}
	return false
}

// isBinary reports whether data looks like a binary file (contains a NUL byte in
// the leading window), so the text tools can skip it.
func isBinary(data []byte) bool {
	if len(data) > binarySniffLen {
		data = data[:binarySniffLen]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// readCapped reads up to maxFileBytes of a file, reporting whether it was
// truncated. Used by the log tools to read a whole (usually large) log. The
// buffer is sized to the file (capped at maxFileBytes+1) so reading a small file
// doesn't allocate the full cap.
func readCapped(abs string) (data []byte, truncated bool, err error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	size := maxFileBytes + 1
	if fi, err := f.Stat(); err == nil {
		if s := fi.Size(); s >= 0 && s < int64(maxFileBytes) {
			size = int(s) + 1
		}
	}
	buf := make([]byte, size)
	n, _ := readFull(f, buf)
	if n > maxFileBytes {
		return buf[:maxFileBytes], true, nil
	}
	return buf[:n], false, nil
}
