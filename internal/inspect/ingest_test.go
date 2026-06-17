package inspect

import (
	"strings"
	"testing"

	"github.com/gilramir/testdiag/internal/knowledge"
	"github.com/gilramir/testdiag/internal/toolproto"
	"github.com/gilramir/testdiag/internal/tools"
)

// grep and search_repo build their matches as []map[string]interface{}, and the
// inspect engine ingests the native tool result with no JSON round-trip. The
// ingester must record those matches rather than treating them as "no matches".
func TestIngestGrepNativeMatches(t *testing.T) {
	store := knowledge.New(0)
	res := &tools.Result{
		Success: true,
		Content: map[string]interface{}{
			"path": "src/foo.go",
			"matches": []map[string]interface{}{
				{"line": 12, "text": "func Bar() {"},
				{"line": 40, "text": "// Bar again"},
			},
			"count": 2,
		},
	}
	c := toolproto.Call{Name: "grep", Args: map[string]interface{}{"path": "src/foo.go", "pattern": "Bar"}}

	ingest(store, c, res, nil)

	out := store.Render()
	if strings.Contains(out, "no matches") {
		t.Fatalf("grep with real hits rendered as \"no matches\":\n%s", out)
	}
	if !strings.Contains(out, "src/foo.go:12") || !strings.Contains(out, "src/foo.go:40") {
		t.Fatalf("expected match line references in render, got:\n%s", out)
	}
}

// search_repo carries a path per match; verify the native shape is ingested too.
func TestIngestSearchRepoNativeMatches(t *testing.T) {
	store := knowledge.New(0)
	res := &tools.Result{
		Success: true,
		Content: map[string]interface{}{
			"matches": []map[string]interface{}{
				{"path": "a/x.go", "line": 3, "text": "hit one"},
				{"path": "b/y.go", "line": 7, "text": "hit two"},
			},
		},
	}
	c := toolproto.Call{Name: "search_repo", Args: map[string]interface{}{"regex": "hit"}}

	ingest(store, c, res, nil)

	out := store.Render()
	if strings.Contains(out, "no matches") {
		t.Fatalf("search_repo with real hits rendered as \"no matches\":\n%s", out)
	}
	if !strings.Contains(out, "a/x.go:3") || !strings.Contains(out, "b/y.go:7") {
		t.Fatalf("expected match line references in render, got:\n%s", out)
	}
}

// A genuinely empty result must still be annotated "no matches".
func TestIngestGrepNoMatches(t *testing.T) {
	store := knowledge.New(0)
	res := &tools.Result{
		Success: true,
		Content: map[string]interface{}{"path": "src/foo.go", "matches": []map[string]interface{}{}, "count": 0},
	}
	c := toolproto.Call{Name: "grep", Args: map[string]interface{}{"path": "src/foo.go", "pattern": "nope"}}

	ingest(store, c, res, nil)

	if out := store.Render(); !strings.Contains(out, "no matches") {
		t.Fatalf("empty grep should render \"no matches\", got:\n%s", out)
	}
}
