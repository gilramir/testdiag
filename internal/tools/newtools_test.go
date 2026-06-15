package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gilramir/testdiag/internal/workspace"
)

// setupWS builds a temporary workspace with a few files and returns it.
func setupWS(t *testing.T) (*workspace.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("client/foo_client.py", "import socket\n\ndef connect():\n    return socket.create_connection((host, port))\n")
	write("server/src/foo.cc", "// foo server\nvoid Bind() { /* race here */ }\n")
	write("node_modules/junk.py", "def connect(): pass\n") // must be skipped
	write(".testdiag/logs/some.log", "line1\nFATAL connect refused\nstack frame a\nstack frame b\nline5\n")

	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws, root
}

func TestSearchRepo(t *testing.T) {
	ws, _ := setupWS(t)
	ResetSearchCache()
	t.Cleanup(ResetSearchCache)
	tool := &searchRepoTool{ws: ws}
	res, err := tool.Execute(context.Background(), map[string]interface{}{"regex": `def connect`})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("not success: %v", res.Error)
	}
	content := res.Content.(map[string]interface{})
	matches := content["matches"].([]map[string]interface{})
	if len(matches) != 1 {
		t.Fatalf("want 1 match (node_modules skipped), got %d: %v", len(matches), matches)
	}
	if matches[0]["path"] != "client/foo_client.py" {
		t.Errorf("wrong path: %v", matches[0]["path"])
	}
	// total and has_more must be present and consistent.
	if content["total"].(int) != 1 {
		t.Errorf("total want 1, got %v", content["total"])
	}
	if content["has_more"].(bool) {
		t.Error("has_more should be false for a single-match result")
	}
}

func TestSearchRepoCaseSensitivity(t *testing.T) {
	ws, _ := setupWS(t)
	ResetSearchCache()
	t.Cleanup(ResetSearchCache)
	tool := &searchRepoTool{ws: ws}

	// Default is case-insensitive: an upper-case regex still matches.
	res, err := tool.Execute(context.Background(), map[string]interface{}{"regex": `DEF CONNECT`})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Content.(map[string]interface{})["matches"].([]map[string]interface{})); n == 0 {
		t.Fatal("default search should be case-insensitive and match 'def connect'")
	}

	// case_sensitive=true: the upper-case regex no longer matches.
	ResetSearchCache()
	res, err = tool.Execute(context.Background(), map[string]interface{}{"regex": `DEF CONNECT`, "case_sensitive": true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Content.(map[string]interface{})["matches"].([]map[string]interface{})); n != 0 {
		t.Fatalf("case_sensitive search should not match 'def connect', got %d matches", n)
	}
}

func TestSearchRepoRefusesLogHuntWhenWithheld(t *testing.T) {
	ws, _ := setupWS(t)
	ResetSearchCache()
	t.Cleanup(ResetSearchCache)
	tool := &searchRepoTool{ws: ws}

	// With logs withheld (DEEPINSPECT), a query naming a log file is refused.
	SetLogToolsEnabled(false)
	defer SetLogToolsEnabled(true)
	for _, q := range []string{"failure.log", "log.txt", "*.log"} {
		res, err := tool.Execute(context.Background(), map[string]interface{}{"regex": q})
		if err == nil || (res != nil && res.Success) {
			t.Errorf("expected refusal for log-hunt pattern %q", q)
		}
	}
	// Also refuse it via the include glob.
	if res, err := tool.Execute(context.Background(), map[string]interface{}{
		"regex": "anything", "include_glob": "*.log",
	}); err == nil || res.Success {
		t.Error("expected refusal for include_glob=*.log")
	}

	// A legitimate source search is NOT refused even when logs are withheld,
	// including a content pattern that merely mentions ".log".
	for _, q := range []string{"def connect", `\.log\(`, "logger"} {
		if res, err := tool.Execute(context.Background(), map[string]interface{}{"regex": q}); err != nil || !res.Success {
			t.Errorf("source search %q should succeed when logs withheld: %v", q, err)
		}
	}

	// When logs are NOT withheld (other contexts), the guard is inactive.
	SetLogToolsEnabled(true)
	if res, err := tool.Execute(context.Background(), map[string]interface{}{"regex": "failure.log"}); err != nil || !res.Success {
		t.Errorf("log-named search should run when logs not withheld: %v", err)
	}
}

func TestSearchRepoExcludesReportDir(t *testing.T) {
	ws, root := setupWS(t)
	ResetSearchCache()
	t.Cleanup(ResetSearchCache)
	// A report directory inside the checkout that holds a generated report
	// mentioning the same symbol; ExcludeDir should keep the search out of it.
	abs := filepath.Join(root, "test-diagnosis", "report.md")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("the root cause is in def connect\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ExcludeDir("test-diagnosis")

	tool := &searchRepoTool{ws: ws}
	res, err := tool.Execute(context.Background(), map[string]interface{}{"regex": `def connect`})
	if err != nil {
		t.Fatal(err)
	}
	matches := res.Content.(map[string]interface{})["matches"].([]map[string]interface{})
	if len(matches) != 1 || matches[0]["path"] != "client/foo_client.py" {
		t.Fatalf("report dir not excluded; matches: %v", matches)
	}
}

func TestSearchRepoPagination(t *testing.T) {
	// Build a workspace with many matching lines so we can page through them.
	root := t.TempDir()
	var body strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&body, "func Hit%d() {}\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "hits.go"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ResetSearchCache()
	t.Cleanup(ResetSearchCache)
	tool := &searchRepoTool{ws: ws}

	// First page: offset=0, limit=4
	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"regex": `func Hit`, "offset": 0, "limit": 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Content.(map[string]interface{})
	if c["count"].(int) != 4 {
		t.Fatalf("page 1: want count=4, got %v", c["count"])
	}
	if c["total"].(int) != 10 {
		t.Fatalf("page 1: want total=10, got %v", c["total"])
	}
	if !c["has_more"].(bool) {
		t.Error("page 1: has_more should be true")
	}

	// Second page: offset=4, limit=4 — must reuse cache (no second walk)
	res2, err := tool.Execute(context.Background(), map[string]interface{}{
		"regex": `func Hit`, "offset": 4, "limit": 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	c2 := res2.Content.(map[string]interface{})
	if c2["count"].(int) != 4 {
		t.Fatalf("page 2: want count=4, got %v", c2["count"])
	}
	if c2["total"].(int) != 10 {
		t.Fatalf("page 2: want total=10 (cached), got %v", c2["total"])
	}
	// Pages must not overlap.
	p1 := res.Content.(map[string]interface{})["matches"].([]map[string]interface{})
	p2 := res2.Content.(map[string]interface{})["matches"].([]map[string]interface{})
	for _, m1 := range p1 {
		for _, m2 := range p2 {
			if m1["line"] == m2["line"] {
				t.Errorf("pages overlap at line %v", m1["line"])
			}
		}
	}

	// Last page: offset=8, limit=4 — only 2 remain
	res3, err := tool.Execute(context.Background(), map[string]interface{}{
		"regex": `func Hit`, "offset": 8, "limit": 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	c3 := res3.Content.(map[string]interface{})
	if c3["count"].(int) != 2 {
		t.Fatalf("last page: want count=2, got %v", c3["count"])
	}
	if c3["has_more"].(bool) {
		t.Error("last page: has_more should be false")
	}
}

func TestSearchRepoCacheIsolation(t *testing.T) {
	ws, _ := setupWS(t)
	ResetSearchCache()
	t.Cleanup(ResetSearchCache)
	tool := &searchRepoTool{ws: ws}

	// Populate cache with one pattern.
	tool.Execute(context.Background(), map[string]interface{}{"regex": `def connect`})

	// A different regex must NOT hit the same cache entry.
	res, err := tool.Execute(context.Background(), map[string]interface{}{"regex": `void Bind`})
	if err != nil {
		t.Fatal(err)
	}
	matches := res.Content.(map[string]interface{})["matches"].([]map[string]interface{})
	if len(matches) != 1 || matches[0]["path"] != "server/src/foo.cc" {
		t.Fatalf("different regex got wrong result: %v", matches)
	}

	// ResetSearchCache must clear both entries so a re-search works cleanly.
	ResetSearchCache()
	res2, _ := tool.Execute(context.Background(), map[string]interface{}{"regex": `def connect`})
	m2 := res2.Content.(map[string]interface{})["matches"].([]map[string]interface{})
	if len(m2) != 1 {
		t.Fatalf("after cache reset: want 1 match, got %d", len(m2))
	}
}

func TestFindFiles(t *testing.T) {
	ws, _ := setupWS(t)
	tool := &findFilesTool{ws: ws}
	res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*_client.py"})
	if err != nil {
		t.Fatal(err)
	}
	paths := res.Content.(map[string]interface{})["paths"].([]string)
	if len(paths) != 1 || paths[0] != "client/foo_client.py" {
		t.Fatalf("want [client/foo_client.py], got %v", paths)
	}

	// substring match
	res, _ = tool.Execute(context.Background(), map[string]interface{}{"pattern": "foo.cc"})
	paths = res.Content.(map[string]interface{})["paths"].([]string)
	if len(paths) != 1 || paths[0] != "server/src/foo.cc" {
		t.Fatalf("substring: want [server/src/foo.cc], got %v", paths)
	}
}

func TestFindFilesCaseInsensitiveByDefault(t *testing.T) {
	ws, _ := setupWS(t)
	ResetFindFilesCache()
	t.Cleanup(ResetFindFilesCache)
	tool := &findFilesTool{ws: ws}

	// Default: case-insensitive, both substring and glob.
	for _, pat := range []string{"FOO_CLIENT.PY", "*CLIENT.PY"} {
		ResetFindFilesCache()
		res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": pat})
		if err != nil {
			t.Fatal(err)
		}
		paths := res.Content.(map[string]interface{})["paths"].([]string)
		if len(paths) != 1 || paths[0] != "client/foo_client.py" {
			t.Fatalf("pattern %q (ci default): want [client/foo_client.py], got %v", pat, paths)
		}
	}

	// case_sensitive=true: the upper-case pattern no longer matches.
	ResetFindFilesCache()
	res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "FOO_CLIENT.PY", "case_sensitive": true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content.(map[string]interface{})["count"].(int) != 0 {
		t.Fatalf("case_sensitive should not match upper-case pattern")
	}
}

func TestFindFilesSameFilenameFallback(t *testing.T) {
	ws, _ := setupWS(t)
	ResetFindFilesCache()
	t.Cleanup(ResetFindFilesCache)
	tool := &findFilesTool{ws: ws}

	// A wrong directory means the primary search finds nothing, but the fallback
	// should surface the file by its base name elsewhere in the tree.
	res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "wrong/place/foo_client.py"})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Content.(map[string]interface{})
	if c["count"].(int) != 0 {
		t.Fatalf("want 0 direct matches, got %v", c["count"])
	}
	same, ok := c["same_filename_matches"].([]string)
	if !ok || len(same) != 1 || same[0] != "client/foo_client.py" {
		t.Fatalf("want same_filename_matches [client/foo_client.py], got %v", c["same_filename_matches"])
	}
	if msg, _ := c["message"].(string); !strings.Contains(msg, "same filename") {
		t.Errorf("message should mention the same-filename fallback, got %q", msg)
	}

	// A truly absent filename yields no fallback candidates.
	ResetFindFilesCache()
	res2, _ := tool.Execute(context.Background(), map[string]interface{}{"pattern": "wrong/place/nope.zzz"})
	if _, present := res2.Content.(map[string]interface{})["same_filename_matches"]; present {
		t.Error("absent filename should not produce same_filename_matches")
	}
}

func TestFindFilesNegativeCache(t *testing.T) {
	ws, _ := setupWS(t)
	ResetFindFilesCache()
	t.Cleanup(ResetFindFilesCache)
	tool := &findFilesTool{ws: ws}

	// First call: pattern that matches nothing → walks the tree, gets 0 results.
	res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*_does_not_exist.java"})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Content.(map[string]interface{})
	if c["count"].(int) != 0 {
		t.Fatalf("want 0 results, got %v", c["count"])
	}
	if _, cached := c["no_results_cached"]; cached {
		t.Error("first call should not carry no_results_cached")
	}

	// Second call with same pattern → served from cache with the don't-retry message.
	res2, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*_does_not_exist.java"})
	if err != nil {
		t.Fatal(err)
	}
	c2 := res2.Content.(map[string]interface{})
	if c2["count"].(int) != 0 {
		t.Fatalf("cached call: want 0 results, got %v", c2["count"])
	}
	if c2["no_results_cached"] != true {
		t.Error("second call should set no_results_cached=true")
	}
	if msg, _ := c2["message"].(string); msg == "" {
		t.Error("second call should include a non-empty message")
	}

	// A different pattern is not affected by the cache.
	res3, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*_client.py"})
	if err != nil {
		t.Fatal(err)
	}
	if res3.Content.(map[string]interface{})["count"].(int) != 1 {
		t.Error("different pattern should still find files")
	}

	// After ResetFindFilesCache the first pattern should walk again (no cached flag).
	ResetFindFilesCache()
	res4, _ := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*_does_not_exist.java"})
	if _, cached := res4.Content.(map[string]interface{})["no_results_cached"]; cached {
		t.Error("after reset, call should not be served from cache")
	}
}

func TestFindFilesPositiveCache(t *testing.T) {
	ws, _ := setupWS(t)
	ResetFindFilesCache()
	t.Cleanup(ResetFindFilesCache)
	tool := &findFilesTool{ws: ws}

	// First call populates the cache.
	res1, _ := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*_client.py"})
	// Second call hits the cache; result must be identical and carry no error message.
	res2, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*_client.py"})
	if err != nil {
		t.Fatal(err)
	}
	c2 := res2.Content.(map[string]interface{})
	if _, bad := c2["no_results_cached"]; bad {
		t.Error("positive cache hit must not set no_results_cached")
	}
	p1 := res1.Content.(map[string]interface{})["paths"].([]string)
	p2 := c2["paths"].([]string)
	if len(p1) != len(p2) || (len(p1) > 0 && p1[0] != p2[0]) {
		t.Errorf("cached result differs: %v vs %v", p1, p2)
	}
}

func TestFindFilesRefusesLogHuntWhenWithheld(t *testing.T) {
	ws, _ := setupWS(t)
	tool := &findFilesTool{ws: ws}

	// With logs withheld (DEEPINSPECT), a lookup naming a log file is refused.
	SetLogToolsEnabled(false)
	defer SetLogToolsEnabled(true)
	for _, q := range []string{"failure.log", "log.txt", "*.log"} {
		res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": q})
		if err == nil || (res != nil && res.Success) {
			t.Errorf("expected refusal for log-hunt pattern %q", q)
		}
	}

	// A legitimate source lookup is NOT refused even when logs are withheld.
	if res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*_client.py"}); err != nil || !res.Success {
		t.Errorf("source lookup should succeed when logs withheld: %v", err)
	}

	// When logs are NOT withheld, the guard is inactive.
	SetLogToolsEnabled(true)
	if res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": "*.log"}); err != nil || !res.Success {
		t.Errorf("log-named lookup should run when logs not withheld: %v", err)
	}
}

func TestReadLogTail(t *testing.T) {
	ws, _ := setupWS(t)
	tool := &readLogTool{ws: ws}
	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"path": ".testdiag/logs/some.log", "tail": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content.(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "4: stack frame b") || !strings.Contains(text, "5: line5") {
		t.Fatalf("tail wrong:\n%s", text)
	}
	if strings.Contains(text, "FATAL") {
		t.Fatalf("tail should not include earlier lines:\n%s", text)
	}
}

func TestLogToolsGate(t *testing.T) {
	ws, _ := setupWS(t)
	args := map[string]interface{}{"path": ".testdiag/logs/some.log", "tail": 2}

	// Disabled: read_log refuses without reading the file.
	SetLogToolsEnabled(false)
	defer SetLogToolsEnabled(true) // restore for other tests (gate defaults on)
	res, err := (&readLogTool{ws: ws}).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected read_log to fail when log tools disabled")
	}
	if res == nil || res.Success {
		t.Fatal("expected an unsuccessful result when disabled")
	}

	gres, gerr := (&grepLogTool{ws: ws}).Execute(context.Background(),
		map[string]interface{}{"path": ".testdiag/logs/some.log", "pattern": "FATAL"})
	if gerr == nil || gres.Success {
		t.Fatal("expected grep_log to fail when log tools disabled")
	}

	// Re-enabled: read_log works again.
	SetLogToolsEnabled(true)
	if _, err := (&readLogTool{ws: ws}).Execute(context.Background(), args); err != nil {
		t.Fatalf("read_log should work when re-enabled: %v", err)
	}
}

func TestGrepLogContext(t *testing.T) {
	ws, _ := setupWS(t)
	tool := &grepLogTool{ws: ws}
	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"path": ".testdiag/logs/some.log", "pattern": "FATAL", "context": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Content.(map[string]interface{})
	if c["count"].(int) != 1 {
		t.Fatalf("want 1 match, got %v", c["count"])
	}
	text := c["text"].(string)
	// match line marked with '>', one line of context on each side with ':'
	if !strings.Contains(text, "2> FATAL connect refused") {
		t.Errorf("missing marked match line:\n%s", text)
	}
	if !strings.Contains(text, "1: line1") || !strings.Contains(text, "3: stack frame a") {
		t.Errorf("missing context lines:\n%s", text)
	}
}

// withConfirmer swaps the run_script approval policy for the duration of a test
// and restores the default afterward.
func withConfirmer(t *testing.T, c Confirmer) {
	t.Helper()
	SetConfirmer(c)
	t.Cleanup(func() { SetConfirmer(nil) })
}

func TestRunScriptApprovedShell(t *testing.T) {
	ws, _ := setupWS(t)
	withConfirmer(t, func(language, script, description string) bool { return true })

	tool := &runScriptTool{ws: ws}
	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"language": "shell",
		"script":   "echo out; echo err 1>&2; exit 3",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Content.(map[string]interface{})
	if c["approved"] != true {
		t.Fatalf("want approved, got %v", c["approved"])
	}
	if c["exit_code"] != 3 {
		t.Errorf("exit_code = %v, want 3", c["exit_code"])
	}
	if !strings.Contains(c["stdout"].(string), "out") {
		t.Errorf("stdout = %q, want it to contain 'out'", c["stdout"])
	}
	if !strings.Contains(c["stderr"].(string), "err") {
		t.Errorf("stderr = %q, want it to contain 'err'", c["stderr"])
	}
}

func TestRunScriptRunsInWorkspace(t *testing.T) {
	ws, root := setupWS(t)
	withConfirmer(t, func(language, script, description string) bool { return true })

	tool := &runScriptTool{ws: ws}
	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"language": "shell",
		"script":   "pwd",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(res.Content.(map[string]interface{})["stdout"].(string))
	// macOS /var -> /private/var symlinks, so compare against the resolved root.
	want, _ := filepath.EvalSymlinks(root)
	if got != want && got != root {
		t.Errorf("cwd = %q, want workspace root %q", got, want)
	}
}

func TestRunScriptDeclinedDoesNotRun(t *testing.T) {
	ws, _ := setupWS(t)
	withConfirmer(t, func(language, script, description string) bool { return false })

	tool := &runScriptTool{ws: ws}
	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"language": "shell",
		// If this ever ran it would create a marker file; we assert it never does.
		"script": "touch SHOULD_NOT_EXIST",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Content.(map[string]interface{})
	if c["approved"] != false {
		t.Fatalf("want approved=false, got %v", c["approved"])
	}
	if _, ok := c["exit_code"]; ok {
		t.Error("declined script should not report an exit_code")
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "SHOULD_NOT_EXIST")); !os.IsNotExist(err) {
		t.Error("declined script must not execute")
	}
}

func TestRunScriptUnsupportedLanguage(t *testing.T) {
	ws, _ := setupWS(t)
	withConfirmer(t, func(language, script, description string) bool {
		t.Fatal("confirmer must not be called for an unsupported language")
		return false
	})
	tool := &runScriptTool{ws: ws}
	res, _ := tool.Execute(context.Background(), map[string]interface{}{
		"language": "ruby",
		"script":   "puts 1",
	})
	if res.Success {
		t.Fatal("want failure for unsupported language")
	}
}

func TestLoopGuardNudgesRepeatedCalls(t *testing.T) {
	ws, _ := setupWS(t)
	ResetLoopGuard()
	t.Cleanup(ResetLoopGuard)

	tool := &loggingTool{inner: &readFileTool{ws: ws}}
	args := map[string]interface{}{"path": "client/foo_client.py"}

	// The first loopThreshold-1 calls execute for real and return file content.
	for i := 1; i < loopThreshold; i++ {
		res, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		c := res.Content.(map[string]interface{})
		if _, looped := c["loop_detected"]; looped {
			t.Fatalf("call %d should have executed, got a nudge", i)
		}
		if _, ok := c["content"]; !ok {
			t.Fatalf("call %d: expected file content", i)
		}
	}

	// The threshold-th identical call is intercepted with a nudge instead.
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	c := res.Content.(map[string]interface{})
	if c["loop_detected"] != true {
		t.Fatalf("call %d should have been nudged, got %v", loopThreshold, c)
	}
	if _, ok := c["content"]; ok {
		t.Error("nudge must not include file content")
	}
}

func TestLoopGuardDistinguishesArgs(t *testing.T) {
	ws, _ := setupWS(t)
	ResetLoopGuard()
	t.Cleanup(ResetLoopGuard)

	tool := &loggingTool{inner: &readFileTool{ws: ws}}
	// Many calls, but each to a different path: never a loop.
	for i := 0; i < loopThreshold+2; i++ {
		path := "client/foo_client.py"
		if i%2 == 0 {
			path = "server/src/foo.cc"
		}
		// Alternating between two distinct calls; neither reaches the threshold
		// until the loopThreshold-th repeat of one of them.
		_, err := tool.Execute(context.Background(), map[string]interface{}{"path": path})
		if err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	// foo.cc was called ceil((threshold+2)/2) times; ensure a fresh, unrelated
	// call still executes (the map is per-fingerprint, not a global counter).
	res, err := tool.Execute(context.Background(), map[string]interface{}{"path": ".testdiag/logs/some.log"})
	if err != nil {
		t.Fatal(err)
	}
	if _, looped := res.Content.(map[string]interface{})["loop_detected"]; looped {
		t.Error("a first-time call must not be treated as a loop")
	}
}

func TestResetLoopGuard(t *testing.T) {
	ws, _ := setupWS(t)
	ResetLoopGuard()
	t.Cleanup(ResetLoopGuard)

	tool := &loggingTool{inner: &readFileTool{ws: ws}}
	args := map[string]interface{}{"path": "client/foo_client.py"}
	for i := 0; i < loopThreshold; i++ {
		tool.Execute(context.Background(), args)
	}
	// After a reset the same call should execute again rather than be nudged.
	ResetLoopGuard()
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if _, looped := res.Content.(map[string]interface{})["loop_detected"]; looped {
		t.Error("ResetLoopGuard did not clear the call history")
	}
}

func TestNotebookAppendAndRead(t *testing.T) {
	ws, _ := setupWS(t)
	SetNotebookPath(".testdiag/notes/test.md")
	t.Cleanup(func() { SetNotebookPath("") })

	tool := &notebookTool{ws: ws}

	// Empty before anything is written.
	res, err := tool.Execute(context.Background(), map[string]interface{}{"action": "read"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content.(map[string]interface{})["empty"] != true {
		t.Fatal("fresh notebook should read as empty")
	}

	// Append two notes.
	for _, note := range []string{"Looking for a race in connect()", "Ruled out: timeout — value is 30s"} {
		res, err = tool.Execute(context.Background(), map[string]interface{}{"action": "append", "note": note})
		if err != nil {
			t.Fatal(err)
		}
		if res.Content.(map[string]interface{})["appended"] != true {
			t.Fatalf("append of %q did not report success", note)
		}
	}

	// Read them back.
	res, err = tool.Execute(context.Background(), map[string]interface{}{"action": "read"})
	if err != nil {
		t.Fatal(err)
	}
	content := res.Content.(map[string]interface{})["content"].(string)
	if !strings.Contains(content, "race in connect()") || !strings.Contains(content, "Ruled out: timeout") {
		t.Fatalf("notebook did not retain both notes:\n%s", content)
	}
}

func TestNotebookRequiresNote(t *testing.T) {
	ws, _ := setupWS(t)
	SetNotebookPath(".testdiag/notes/test.md")
	t.Cleanup(func() { SetNotebookPath("") })

	tool := &notebookTool{ws: ws}
	res, _ := tool.Execute(context.Background(), map[string]interface{}{"action": "append"})
	if res.Success {
		t.Fatal("append without a note should fail")
	}
}

func TestNotebookDisabledWhenUnset(t *testing.T) {
	ws, _ := setupWS(t)
	SetNotebookPath("")

	tool := &notebookTool{ws: ws}
	res, _ := tool.Execute(context.Background(), map[string]interface{}{"action": "read"})
	if res.Success {
		t.Fatal("notebook should be unavailable when no path is set")
	}
}

func TestNotebookExemptFromLoopGuard(t *testing.T) {
	ws, _ := setupWS(t)
	SetNotebookPath(".testdiag/notes/test.md")
	ResetLoopGuard()
	t.Cleanup(func() { SetNotebookPath(""); ResetLoopGuard() })

	tool := &loggingTool{inner: &notebookTool{ws: ws}}
	args := map[string]interface{}{"action": "read"}
	// Far more identical reads than loopThreshold: the notebook must never be
	// intercepted with a loop nudge.
	for i := 0; i < loopThreshold+3; i++ {
		res, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		if _, looped := res.Content.(map[string]interface{})["loop_detected"]; looped {
			t.Fatalf("notebook read %d was wrongly treated as a loop", i+1)
		}
	}
}
