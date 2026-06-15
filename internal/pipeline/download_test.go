package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gilramir/testdiag/internal/jenkins"
	"github.com/gilramir/testdiag/internal/workspace"
)

func TestDownloadStageWritesLog(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	test := jenkins.FailedTest{
		ClassName:       "pkg.FooTest",
		Name:            "testBar",
		Status:          "FAILED",
		ErrorDetails:    "boom",
		ErrorStackTrace: "at pkg.FooTest.testBar(FooTest.java:42)",
		Stdout:          "starting up",
	}

	sc := &Context{Test: test}
	if err := (&downloadStage{ws: ws}).Run(context.Background(), sc); err != nil {
		t.Fatalf("download Run: %v", err)
	}

	wantRel := filepath.ToSlash(filepath.Join(logDir, "pkg.FooTest.testBar.log"))
	if sc.LogPath != wantRel {
		t.Errorf("LogPath = %q, want %q", sc.LogPath, wantRel)
	}

	data, err := os.ReadFile(filepath.Join(root, sc.LogPath))
	if err != nil {
		t.Fatalf("reading saved log: %v", err)
	}
	body := string(data)
	for _, want := range []string{"ERROR DETAILS", "boom", "STACK TRACE", "FooTest.java:42", "STDOUT", "starting up"} {
		if !strings.Contains(body, want) {
			t.Errorf("saved log missing %q; got:\n%s", want, body)
		}
	}
}

func TestMakeExcerptElidesMiddle(t *testing.T) {
	var lines []string
	for i := 0; i < 1000; i++ {
		lines = append(lines, "line")
	}
	lines[0] = "HEAD-MARKER"
	lines[999] = "TAIL-MARKER"
	out := makeExcerpt(strings.Join(lines, "\n"), 10, 10)

	if !strings.Contains(out, "HEAD-MARKER") || !strings.Contains(out, "TAIL-MARKER") {
		t.Error("excerpt should keep head and tail markers")
	}
	if !strings.Contains(out, "lines omitted") {
		t.Error("excerpt should note omitted middle lines")
	}
	if got := strings.Count(out, "\n"); got > 25 {
		t.Errorf("excerpt too long: %d lines", got)
	}

	// A short log is returned whole, untouched.
	short := "a\nb\nc"
	if out := makeExcerpt(short, 10, 10); out != short {
		t.Errorf("short log changed: %q", out)
	}
}
