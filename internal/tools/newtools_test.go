package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gilbertr/testdiag/internal/workspace"
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
	tool := &searchRepoTool{ws: ws}
	res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": `def connect`})
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
}

func TestSearchRepoExcludesReportDir(t *testing.T) {
	ws, root := setupWS(t)
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
	res, err := tool.Execute(context.Background(), map[string]interface{}{"pattern": `def connect`})
	if err != nil {
		t.Fatal(err)
	}
	matches := res.Content.(map[string]interface{})["matches"].([]map[string]interface{})
	if len(matches) != 1 || matches[0]["path"] != "client/foo_client.py" {
		t.Fatalf("report dir not excluded; matches: %v", matches)
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

func TestGitBlameAndLog(t *testing.T) {
	ws, root := setupWS(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	runIn := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=t@e.x",
			"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=t@e.x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runIn("init", "-q")
	runIn("add", ".")
	runIn("commit", "-qm", "initial import of client and server")

	blame := &gitBlameTool{ws: ws}
	res, err := blame.Execute(context.Background(), map[string]interface{}{
		"path": "client/foo_client.py", "start": 3, "end": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("blame failed: %v", res.Error)
	}
	if !strings.Contains(res.Content.(map[string]interface{})["blame"].(string), "Tester") {
		t.Errorf("blame missing author:\n%v", res.Content)
	}

	lg := &gitLogTool{ws: ws}
	res, err = lg.Execute(context.Background(), map[string]interface{}{"path": "server/src/foo.cc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content.(map[string]interface{})["log"].(string), "initial import") {
		t.Errorf("log missing commit subject:\n%v", res.Content)
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
	withConfirmer(t, func(language, script string) bool { return true })

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
	withConfirmer(t, func(language, script string) bool { return true })

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
	withConfirmer(t, func(language, script string) bool { return false })

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
	withConfirmer(t, func(language, script string) bool {
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
