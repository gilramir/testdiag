package pipeline

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gilramir/testdiag/internal/workspace"
)

func TestExtractPlanFiles(t *testing.T) {
	plan := `## Inspection Plan for Hypothesis 1: races

### High Priority
- ` + "`src/server/Foo.java`" + ` [lines 42-80 | grep: ` + "`acquireLock`" + `] — owns the lock
-   ` + "`src/server/Bar.java`" + ` — secondary path
* ` + "`pkg/util/race.go`" + ` [grep: ` + "`go func`" + `] — goroutine spawn

### Low Priority
- A prose bullet with no path at all
- Symbol reference ` + "`SomeClass`" + ` should be skipped? actually leading token
- ` + "`src/server/Foo.java`" + ` — duplicate, should dedupe
`
	got := extractPlanFiles(plan)
	want := []string{
		"src/server/Foo.java",
		"src/server/Bar.java",
		"pkg/util/race.go",
		"SomeClass",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractPlanFiles:\n got %#v\nwant %#v", got, want)
	}
}

func TestMissingPlanFiles(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"src/real.go", "pkg/also_real.go"} {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}

	plan := "### High Priority\n" +
		"- `src/real.go` — exists\n" +
		"- `src/ghost.go` — does not exist\n" +
		"- `pkg/*.go` — glob that matches\n" +
		"- `cmd/*.go` — glob that matches nothing\n"

	got := missingPlanFiles(ws, plan)
	want := []string{"src/ghost.go", "cmd/*.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missingPlanFiles:\n got %#v\nwant %#v", got, want)
	}
}
