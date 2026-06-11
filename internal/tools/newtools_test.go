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
