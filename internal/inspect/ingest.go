package inspect

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gilbertr/testdiag/internal/knowledge"
	"github.com/gilbertr/testdiag/internal/toolproto"
	"github.com/gilbertr/testdiag/internal/tools"
)

// ingest folds the result of one tool call into the knowledge store. Each
// tool's structured result is mapped to the store's file/search records so the
// model sees coalesced, deduplicated facts on the next turn. Any tool not
// specially handled (and any failure) falls through to a generic recorder so
// nothing the agent learned is ever silently dropped.
func ingest(store *knowledge.Store, c toolproto.Call, res *tools.Result, err error) {
	if err != nil || res == nil || !res.Success {
		recordFailure(store, c, res, err)
		return
	}
	m, _ := res.Content.(map[string]interface{})

	switch c.Name {
	case "read_file":
		path := strv(m, "path", strv(c.Args, "path", ""))
		store.AddWholeFile(path, strv(m, "content", ""))
		if boolv(m, "truncated") {
			store.SetFileNote(path, "truncated — only the first part was read")
		}

	case "read_lines":
		path := strv(m, "path", strv(c.Args, "path", ""))
		start := intv(m, "start", intv(c.Args, "start", 1))
		store.AddLines(path, start, stripLineNumbers(strv(m, "text", "")))

	case "grep":
		path := strv(m, "path", strv(c.Args, "path", ""))
		label := fmt.Sprintf("`%s` in %s", strv(c.Args, "pattern", ""), path)
		results := ingestMatches(store, path, m)
		store.AddSearch("grep", label, results)
		if len(results) == 0 {
			store.SetSearchNote("grep", label, "no matches")
		}

	case "search_repo":
		label := fmt.Sprintf("`%s`", strv(c.Args, "regex", strv(c.Args, "pattern", "")))
		results := ingestMatches(store, "", m)
		store.AddSearch("search_repo", label, results)
		if len(results) == 0 {
			store.SetSearchNote("search_repo", label, "no matches")
		} else if boolv(m, "has_more") {
			store.SetSearchNote("search_repo", label, "more matches exist (paged result)")
		}

	case "find_files":
		label := strv(c.Args, "pattern", "")
		if paths := strSlice(m, "paths"); len(paths) > 0 {
			store.AddSearch("find_files", label, paths)
		} else if same := strSlice(m, "same_filename_matches"); len(same) > 0 {
			// No direct hit, but files with the same name exist elsewhere — record
			// them as candidates so the agent can pick the right path.
			store.AddSearch("find_files", label, same)
			store.SetSearchNote("find_files", label, "no direct match; files with the same filename found elsewhere:")
		} else {
			store.AddSearch("find_files", label, nil)
			store.SetSearchNote("find_files", label, "no matches anywhere")
		}

	case "file_exists":
		path := strv(m, "path", strv(c.Args, "path", ""))
		if boolv(m, "exists") {
			note := "exists"
			if boolv(m, "is_dir") {
				note = "directory"
			}
			store.SetFileNote(path, note)
		} else {
			store.MarkNotFound(path)
		}

	case "list_directory":
		path := strv(c.Args, "path", ".")
		store.AddSearch("list_directory", path, strSlice(m, "entries"))

	default:
		recordGeneric(store, c, res)
	}
}

// ingestMatches folds match records ({path?, line, text}) into the store as
// per-file content and returns "path:line" result lines. fixedPath is used when
// the matches carry no path of their own (grep, which is single-file).
func ingestMatches(store *knowledge.Store, fixedPath string, m map[string]interface{}) []string {
	raw, _ := m["matches"].([]interface{})
	var results []string
	for _, item := range raw {
		mm, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		path := strv(mm, "path", fixedPath)
		line := intv(mm, "line", 0)
		text := strv(mm, "text", "")
		if path != "" && line > 0 {
			store.AddMatch(path, line, text)
			results = append(results, fmt.Sprintf("%s:%d", path, line))
		}
	}
	return results
}

// recordFailure notes a failed or errored call so the model sees that it failed
// (and why) and does not blindly retry it.
func recordFailure(store *knowledge.Store, c toolproto.Call, res *tools.Result, err error) {
	msg := "failed"
	switch {
	case err != nil:
		msg = "error: " + err.Error()
	case res != nil && res.Error != "":
		msg = "error: " + res.Error
	}
	label := argsLabel(c.Args)
	store.AddSearch(c.Name, label, nil)
	store.SetSearchNote(c.Name, label, truncate(msg, 300))
}

// recordGeneric stores the result of a tool we don't specially structure (e.g.
// git_blame, git_log, count_lines) as a search record carrying a compact
// summary, so its output is never lost.
func recordGeneric(store *knowledge.Store, c toolproto.Call, res *tools.Result) {
	label := argsLabel(c.Args)
	store.AddSearch(c.Name, label, nil)
	store.SetSearchNote(c.Name, label, truncate(summarize(res.Content), 1500))
}

var lineNumPrefix = regexp.MustCompile(`^\s*\d+:\s?`)

// stripLineNumbers removes the "N: " prefix read_lines adds to each line,
// recovering the raw file content for storage.
func stripLineNumbers(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		lines[i] = lineNumPrefix.ReplaceAllString(ln, "")
	}
	return lines
}

// argsLabel renders a call's arguments as a stable, compact "k=v" string used
// as a dedup key and display label for generic/failed calls.
func argsLabel(args map[string]interface{}) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, args[k]))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func summarize(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]interface{}:
		if s, ok := x["text"].(string); ok {
			return s
		}
		if s, ok := x["content"].(string); ok {
			return s
		}
		var b strings.Builder
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s: %v\n", k, x[k])
		}
		return b.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// --- small typed accessors over the untyped tool-result maps ---

func strv(m map[string]interface{}, key, def string) string {
	if m == nil {
		return def
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return def
}

func intv(m map[string]interface{}, key string, def int) int {
	if m == nil {
		return def
	}
	switch v := m[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return def
}

func boolv(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

func strSlice(m map[string]interface{}, key string) []string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
