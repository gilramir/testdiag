package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/gilramir/testdiag/internal/workspace"
)

// Caps for the tree-walking tools. They protect both the context window and the
// wall-clock cost of crawling a large checkout.
const (
	maxRepoMatches   = 200   // max matches returned per page by search_repo (no offset/limit)
	maxRepoCacheSize = 2000  // max matches stored in the full search cache
	maxFindResults   = 500   // max paths returned by find_files
	maxFilesScanned  = 20000 // max files visited by a single walk
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
// search_repo result cache
// ---------------------------------------------------------------------------

// searchCacheEntry holds the full match list for one (regex, path, include, ignoreCase)
// combination so that paginated calls don't redo the walk.
type searchCacheEntry struct {
	matches   []map[string]interface{}
	truncated bool // true if the walk hit maxRepoCacheSize before scanning all files
}

var (
	searchCacheMu    sync.Mutex
	searchCacheStore = map[string]*searchCacheEntry{}
)

// searchCacheKey builds the map key from the parameters that define a unique
// search. offset and limit are intentionally excluded — same search, different page.
func searchCacheKey(pattern, base, include string, ignoreCase bool) string {
	ic := "0"
	if ignoreCase {
		ic = "1"
	}
	return pattern + "\x00" + base + "\x00" + include + "\x00" + ic
}

// ResetSearchCache discards all cached search_repo results. Call at the start
// of each agent run (alongside ResetLoopGuard) so results from one hypothesis
// don't bleed into the next.
func ResetSearchCache() {
	searchCacheMu.Lock()
	searchCacheStore = map[string]*searchCacheEntry{}
	searchCacheMu.Unlock()
}

// ---------------------------------------------------------------------------
// search_repo (recursive grep across the workspace tree)
// ---------------------------------------------------------------------------

type searchRepoTool struct{ ws *workspace.Workspace }

func (t *searchRepoTool) Name() string { return "search_repo" }
func (t *searchRepoTool) Description() string {
	return "Recursively search the workspace tree for a regular-expression pattern and return matching lines. Matching is case-insensitive by default (set case_sensitive=true to require an exact-case match). Results are cached after the first call: repeating the same search (same regex, path, include_glob, case_sensitive) with a different offset/limit is instant and does NOT re-scan the tree. Use offset+limit to page through a large result set rather than repeating the search. This crawls the WHOLE tree and can be slow, so prefer narrower lookups first: if you already know a symbol is imported, follow its import to the defining file and grep that file instead; and when you must search, pass an include_glob (e.g. *.py) and search for the definition (e.g. 'def name'/'class name') rather than every use. Skips VCS, dependency, and build directories and binary files."
}
func (t *searchRepoTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"regex":          map[string]interface{}{"type": "string", "description": "RE2 regular expression to match against each line."},
			"path":           map[string]interface{}{"type": "string", "description": "Workspace-relative directory to limit the search to. Defaults to the whole workspace ('.')."},
			"include_glob":   map[string]interface{}{"type": "string", "description": "Optional filename glob to restrict which files are searched (e.g. *.py or *Test.java)."},
			"case_sensitive": map[string]interface{}{"type": "boolean", "description": "Require an exact-case match. Defaults to false (matching is case-insensitive)."},
			"offset":         map[string]interface{}{"type": "integer", "description": "0-based index of the first match to return. Use with limit to page through large result sets. The result set is cached after the first call, so paging is free."},
			"limit":          map[string]interface{}{"type": "integer", "description": "Maximum number of matches to return. Defaults to all matches from offset onward (up to the per-call cap when no offset is given)."},
		},
		"required": []string{"regex"},
	}
}
func (t *searchRepoTool) Execute(ctx context.Context, args map[string]interface{}) (*Result, error) {
	pattern, has := strArg(args, "regex")
	if !has {
		return fail("search_repo: 'regex' is required")
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

	// Matching is case-insensitive by default; case_sensitive=true opts out.
	ignoreCase := !boolArg(args, "case_sensitive")
	expr := pattern
	if ignoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return fail("search_repo: invalid regex: %v", err)
	}
	base := "."
	if p, ok := strArg(args, "path"); ok {
		base = p
	}
	include, _ := strArg(args, "include_glob")

	root, err := t.ws.Resolve(base)
	if err != nil {
		return fail("search_repo: %v", err)
	}

	// Check the cache; populate on miss.
	cacheKey := searchCacheKey(pattern, base, include, ignoreCase)
	entry := t.cacheGet(cacheKey)
	if entry == nil {
		entry = t.populateCache(ctx, cacheKey, root, include, re)
		if entry == nil {
			return fail("search_repo: %v", ctx.Err())
		}
	}

	// Apply offset/limit to the cached full result set.
	all := entry.matches
	total := len(all)

	offset := 0
	if v, ok := intArg(args, "offset"); ok && v > 0 {
		offset = v
	}
	if offset > total {
		offset = total
	}

	end := total
	if v, ok := intArg(args, "limit"); ok && v > 0 {
		if end = offset + v; end > total {
			end = total
		}
	} else if offset == 0 {
		// No pagination requested: honour the legacy per-call cap so the first
		// call without offset/limit returns a manageable slice.
		if end > maxRepoMatches {
			end = maxRepoMatches
		}
	}

	page := all[offset:end]
	return ok(map[string]interface{}{
		"matches":   page,
		"count":     len(page),
		"total":     total,
		"offset":    offset,
		"has_more":  end < total,
		"truncated": entry.truncated,
	}), nil
}

// cacheGet returns the cached entry for key, or nil if not present.
func (t *searchRepoTool) cacheGet(key string) *searchCacheEntry {
	searchCacheMu.Lock()
	defer searchCacheMu.Unlock()
	return searchCacheStore[key]
}

// populateCache performs the full walk, stores the result under key, and
// returns the new entry. Returns nil if the context was cancelled mid-walk.
func (t *searchRepoTool) populateCache(ctx context.Context, key, root, include string, re *regexp.Regexp) *searchCacheEntry {
	hasInclude := include != ""
	var matches []map[string]interface{}
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

		if searchFile(abs, t.ws.Rel(abs), re, &matches, maxRepoCacheSize) {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return nil
	}

	entry := &searchCacheEntry{matches: matches, truncated: truncated}
	searchCacheMu.Lock()
	searchCacheStore[key] = entry
	searchCacheMu.Unlock()
	return entry
}

// logFileQueryRe matches a query whose ENTIRETY names a log file — e.g.
// "failure.log", "log.txt", "*.log", "logs/app.log" — as opposed to a content
// search that merely mentions ".log" (like the pattern "\.log\("), which is left
// alone. The whole-string anchors keep it from flagging legitimate source
// searches.
var logFileQueryRe = regexp.MustCompile(`(?i)^[\w./*?-]*?([\w*?-]*\.log|log\.txt)$`)

// logHuntQuery returns the first argument (across both search_repo and
// find_files) that looks like an attempt to locate a log file, or "" if none
// does. It checks every key either tool can receive so the guard works
// regardless of which tool calls it.
func logHuntQuery(args map[string]interface{}) string {
	for _, key := range []string{"regex", "pattern", "include_glob", "path"} {
		if v, ok := strArg(args, key); ok && logFileQueryRe.MatchString(strings.TrimSpace(v)) {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// find_files result cache (negative entries)
// ---------------------------------------------------------------------------

// findFilesCacheEntry holds the result of one find_files walk so that a
// repeated call with the same arguments can skip the walk entirely. Empty
// entries (no files matched) are specifically worth caching because the LLM
// often retries the same pattern after getting no results.
type findFilesCacheEntry struct {
	paths     []string
	truncated bool
	// sameName holds the fallback result computed when paths is empty: files
	// anywhere in the workspace whose base name matches the non-directory part of
	// the pattern, so a repeat of a zero-result call still offers the same hints.
	sameName []string
}

var (
	findFilesCacheMu    sync.Mutex
	findFilesCacheStore = map[string]*findFilesCacheEntry{}
)

func findFilesCacheKey(pattern, base string, ignoreCase bool) string {
	ic := "0"
	if ignoreCase {
		ic = "1"
	}
	return pattern + "\x00" + base + "\x00" + ic
}

// ResetFindFilesCache discards all cached find_files results. Call at the
// start of each agent run alongside ResetSearchCache and ResetLoopGuard.
func ResetFindFilesCache() {
	findFilesCacheMu.Lock()
	findFilesCacheStore = map[string]*findFilesCacheEntry{}
	findFilesCacheMu.Unlock()
}

// ---------------------------------------------------------------------------
// find_files (locate files by name / glob across the tree)
// ---------------------------------------------------------------------------

type findFilesTool struct{ ws *workspace.Workspace }

func (t *findFilesTool) Name() string { return "find_files" }
func (t *findFilesTool) Description() string {
	return "Find files in the workspace whose name matches a glob (e.g. *Test.java, foo_client.py) or whose path contains a substring. Returns workspace-relative paths. Matching is case-insensitive by default (set case_sensitive=true to require an exact-case match). If nothing matches, the directory part of the pattern is dropped and the workspace is searched for any file with that filename anywhere — those are returned under same_filename_matches so you can pick the right one. Use this to locate a test's source file instead of crawling directories by hand. Results are cached: a repeated zero-result call returns the same answer immediately — do not retry the same pattern."
}
func (t *findFilesTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern":        map[string]interface{}{"type": "string", "description": "Filename glob (e.g. *Test.java) matched against each file's name, or a plain substring matched against the full path."},
			"path":           map[string]interface{}{"type": "string", "description": "Workspace-relative directory to limit the search to. Defaults to the whole workspace ('.')."},
			"case_sensitive": map[string]interface{}{"type": "boolean", "description": "Require an exact-case match. Defaults to false (matching is case-insensitive)."},
		},
		"required": []string{"pattern"},
	}
}
func (t *findFilesTool) Execute(ctx context.Context, args map[string]interface{}) (*Result, error) {
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
	// Matching is case-insensitive by default; case_sensitive=true opts out.
	ci := !boolArg(args, "case_sensitive")

	// Check the cache before resolving the path or walking the tree.
	cacheKey := findFilesCacheKey(pattern, base, ci)
	findFilesCacheMu.Lock()
	cached, hit := findFilesCacheStore[cacheKey]
	findFilesCacheMu.Unlock()

	if hit {
		if len(cached.paths) == 0 {
			return zeroResult(cached.sameName, true), nil
		}
		return ok(map[string]interface{}{
			"paths":     cached.paths,
			"count":     len(cached.paths),
			"truncated": cached.truncated,
		}), nil
	}

	root, err := t.ws.Resolve(base)
	if err != nil {
		return fail("find_files: %v", err)
	}

	paths, truncated, err := t.walkMatch(ctx, root, func(rel, name string) bool {
		return matchPath(pattern, rel, name, ci)
	})
	if err != nil {
		return fail("find_files: %v", err)
	}

	// Zero results: drop the directory part of the pattern and look for any file
	// with that filename anywhere in the workspace, so the model gets candidate
	// paths instead of a dead end.
	var sameName []string
	if len(paths) == 0 {
		sameName = t.findSameFilename(ctx, pattern, base)
	}

	// Store result in cache (including empty results — that's the main point).
	findFilesCacheMu.Lock()
	findFilesCacheStore[cacheKey] = &findFilesCacheEntry{paths: paths, truncated: truncated, sameName: sameName}
	findFilesCacheMu.Unlock()

	if len(paths) == 0 {
		return zeroResult(sameName, false), nil
	}
	return ok(map[string]interface{}{
		"paths":     paths,
		"count":     len(paths),
		"truncated": truncated,
	}), nil
}

// walkMatch walks root and returns the workspace-relative paths of files for
// which match(rel, baseName) is true, honoring the same dir-skipping and caps as
// the primary find_files walk.
func (t *findFilesTool) walkMatch(ctx context.Context, root string, match func(rel, name string) bool) (paths []string, truncated bool, err error) {
	filesScanned := 0
	walkErr := filepath.WalkDir(root, func(abs string, d fs.DirEntry, werr error) error {
		if werr != nil {
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
		if match(rel, d.Name()) {
			if len(paths) >= maxFindResults {
				truncated = true
				return filepath.SkipAll
			}
			paths = append(paths, rel)
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	sort.Strings(paths)
	return paths, truncated, nil
}

// findSameFilename implements the zero-result fallback: it takes the
// non-directory part of the pattern and searches the WHOLE workspace for files
// whose base name matches it. It returns nil when the fallback could not differ
// from the primary search (a directory-less pattern already searched from the
// workspace root), and is always case-insensitive so a near-miss on case still
// surfaces candidates.
func (t *findFilesTool) findSameFilename(ctx context.Context, pattern, base string) []string {
	needle := path.Base(strings.TrimPrefix(pattern, "**/"))
	hadDir := strings.Contains(strings.TrimPrefix(pattern, "**/"), "/")
	if needle == "" || needle == "." {
		return nil
	}
	// If the pattern has no directory part and we already searched from the root,
	// a base-name search over the root would repeat the primary search.
	if !hadDir && base == "." {
		return nil
	}
	root, err := t.ws.Resolve(".")
	if err != nil {
		return nil
	}
	paths, _, err := t.walkMatch(ctx, root, func(_, name string) bool {
		return matchBaseName(needle, name, true)
	})
	if err != nil {
		return nil
	}
	return paths
}

// zeroResult builds the find_files response for a search that matched nothing,
// attaching any same-filename candidates found elsewhere in the workspace. When
// cached is true the result is being replayed from the negative cache, so it
// also tells the model not to retry.
func zeroResult(sameName []string, cached bool) *Result {
	msg := "No files matched this pattern."
	if cached {
		msg += " (Served from a previous search — do not retry the same arguments.)"
	}
	if len(sameName) > 0 {
		msg += fmt.Sprintf(" However, %d file(s) with the same filename exist elsewhere in the workspace "+
			"(see same_filename_matches) — one of these may be the file you want.", len(sameName))
	}
	out := map[string]interface{}{
		"paths":     []string{},
		"count":     0,
		"truncated": false,
		"message":   msg,
	}
	if cached {
		out["no_results_cached"] = true
	}
	if len(sameName) > 0 {
		out["same_filename_matches"] = sameName
	}
	return ok(out)
}

// matchPath reports whether a file matches the find_files pattern. A pattern
// containing glob metacharacters is matched (via filepath.Match) against both
// the base name and the relative path, tolerating a leading "**/"; a plain
// pattern is matched as a case-optional substring of the path.
func matchPath(pattern, rel, base string, ci bool) bool {
	p := strings.TrimPrefix(pattern, "**/")
	if strings.ContainsAny(pattern, "*?[") {
		// filepath.Match has no case-insensitive mode; fold both sides when ci.
		full, p2, b, r := pattern, p, base, rel
		if ci {
			full, p2, b, r = strings.ToLower(pattern), strings.ToLower(p), strings.ToLower(base), strings.ToLower(rel)
		}
		if m, _ := filepath.Match(p2, b); m {
			return true
		}
		if m, _ := filepath.Match(full, r); m {
			return true
		}
		if m, _ := filepath.Match(p2, r); m {
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

// matchBaseName reports whether a file's base name matches the non-directory
// needle used by the find_files zero-result fallback: a glob needle is matched
// against the base name with filepath.Match; a plain needle matches when the
// base name contains it. Case-insensitive when ci.
func matchBaseName(needle, base string, ci bool) bool {
	n := strings.TrimPrefix(needle, "**/")
	if strings.ContainsAny(n, "*?[") {
		pat, b := n, base
		if ci {
			pat, b = strings.ToLower(n), strings.ToLower(base)
		}
		m, _ := filepath.Match(pat, b)
		return m
	}
	nd, hay := n, base
	if ci {
		nd, hay = strings.ToLower(n), strings.ToLower(base)
	}
	return strings.Contains(hay, nd)
}

// binarySniffLen is how many leading bytes searchFile inspects to decide a file
// is binary before scanning it line by line.
const binarySniffLen = 8000

// searchFile scans one file line by line for re, appending matches (capped at
// cap across the whole walk) to *matches. It STREAMS the file with a small
// buffer — reading at most maxFileBytes — rather than loading the whole file
// into memory, and skips files that look binary from their first bytes.
// Returns true when the cap is reached so the walk can stop early.
//
// This is the hot path of search_repo: it runs once per regular file in the
// tree, so it must not allocate per-file buffers proportional to maxFileBytes.
func searchFile(abs, rel string, re *regexp.Regexp, matches *[]map[string]interface{}, cap int) (capped bool) {
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
		if len(*matches) >= cap {
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
