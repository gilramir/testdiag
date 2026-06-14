package inspect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// scriptedClient returns a canned response per turn and records the user
// messages it was sent, so a test can assert the accumulated knowledge is
// re-presented each turn.
type scriptedClient struct {
	responses []string
	turn      int
	users     []string
}

func (c *scriptedClient) Chat(ctx context.Context, system, user string, schemas []tools.Schema) (string, error) {
	c.users = append(c.users, user)
	r := ""
	if c.turn < len(c.responses) {
		r = c.responses[c.turn]
	}
	c.turn++
	return r, nil
}

func TestEngineDrivesToolsAndAccumulates(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/lock.go", "package lock\nfunc Acquire() {}\nfunc Release() {}\n")
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	tools.Register(ws)
	tools.ResetLoopGuard()

	client := &scriptedClient{responses: []string{
		// Turn 1: locate the symbol.
		`TOOL_CALL{"name":"search_repo","args":{"regex":"Acquire"}}`,
		// Turn 2: read the file the search found.
		`TOOL_CALL{"name":"read_lines","args":{"path":"src/lock.go","start":1,"end":3}}`,
		// Turn 3: final answer, no tools.
		"## Verdict\nCONFIRMED. Acquire is defined at src/lock.go:2.",
	}}

	eng := newEngineWithClient(client, Options{MaxIterations: 5, Schemas: tools.Schemas()})
	res, err := eng.Run(context.Background(), RunInput{System: "sys", Task: "Find where Acquire is defined."})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(res.Content, "CONFIRMED") {
		t.Errorf("expected final verdict, got: %q", res.Content)
	}
	if res.Iterations != 2 {
		t.Errorf("expected 2 tool rounds, got %d", res.Iterations)
	}
	if got := strings.Join(res.ToolsCalled, ","); got != "search_repo,read_lines" {
		t.Errorf("tools called = %q", got)
	}

	// The store should hold the file content read in turn 2.
	dump := res.Store.Render()
	if !strings.Contains(dump, "src/lock.go") || !strings.Contains(dump, "func Acquire()") {
		t.Errorf("store missing read content:\n%s", dump)
	}

	// Turn 2's prompt must already contain the search results from turn 1,
	// proving facts accumulate across turns (the whole point).
	if len(client.users) < 2 || !strings.Contains(client.users[1], "search_repo") {
		t.Errorf("turn 2 prompt did not carry forward turn 1's search:\n%s", client.users[1])
	}
}

func TestEngineForcesFinalAnswerAtBudget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "hello\n")
	ws, _ := workspace.New(root)
	tools.Register(ws)
	tools.ResetLoopGuard()

	// Model never stops calling tools on its own.
	client := &scriptedClient{responses: []string{
		`TOOL_CALL{"name":"read_file","args":{"path":"a.txt"}}`,
		`TOOL_CALL{"name":"read_file","args":{"path":"a.txt"}}`,
		"Final answer after being forced.",
	}}

	eng := newEngineWithClient(client, Options{MaxIterations: 2, Schemas: tools.Schemas()})
	res, err := eng.Run(context.Background(), RunInput{System: "sys", Task: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Final answer") {
		t.Errorf("expected forced final answer, got %q", res.Content)
	}
	// The forced turn must advertise no tools.
	last := client.users[len(client.users)-1]
	if !strings.Contains(last, "Do NOT request any more tools") {
		t.Errorf("final prompt missing the no-tools instruction:\n%s", last)
	}
}

func TestEngineRecordsNotFound(t *testing.T) {
	root := t.TempDir()
	ws, _ := workspace.New(root)
	tools.Register(ws)
	tools.ResetLoopGuard()

	client := &scriptedClient{responses: []string{
		`TOOL_CALL{"name":"file_exists","args":{"path":"ghost.go"}}`,
		"done",
	}}
	eng := newEngineWithClient(client, Options{MaxIterations: 5, Schemas: tools.Schemas()})
	res, _ := eng.Run(context.Background(), RunInput{System: "s", Task: "t"})
	if !strings.Contains(res.Store.Render(), "NOT FOUND") {
		t.Errorf("expected ghost.go recorded NOT FOUND:\n%s", res.Store.Render())
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
