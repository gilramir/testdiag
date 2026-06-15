package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gilramir/testdiag/internal/jenkins"
	"github.com/gilramir/testdiag/internal/workspace"
)

// logDir is where fetched failure logs are written, relative to the workspace
// root, so the jailed file tools (LOGPARSE excerpts it; DEEPINSPECT does not get
// it) can reach the raw log.
const logDir = ".testdiag/logs"

// downloadStage materializes the failing test's combined output to disk. The
// Jenkins API call that enumerates failures happens once up front in main; this
// stage just writes the per-test log that LOGPARSE will read.
type downloadStage struct {
	ws      *workspace.Workspace
	verbose bool
}

func (s *downloadStage) Name() State { return StateDownload }

func (s *downloadStage) Run(ctx context.Context, sc *Context) error {
	stageBanner(s.verbose, string(s.Name()), 1)
	rel, err := saveLog(s.ws.Root(), sc.Test)
	if err != nil {
		return err
	}
	sc.LogPath = rel
	return nil
}

// saveLog writes the test's combined output under <root>/.testdiag/logs and
// returns the workspace-relative path (so the jailed tools can open it).
func saveLog(root string, test jenkins.FailedTest) (string, error) {
	dir := filepath.Join(root, logDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	rel := filepath.Join(logDir, sanitize(test.FullName())+".log")
	abs := filepath.Join(root, rel)
	if err := os.WriteFile(abs, []byte(combinedLog(test)), 0o644); err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// combinedLog assembles the full failure output the way a developer would see
// it: error summary, stack trace, then captured stdout/stderr.
func combinedLog(test jenkins.FailedTest) string {
	var b strings.Builder
	section := func(title, body string) {
		if strings.TrimSpace(body) == "" {
			return
		}
		fmt.Fprintf(&b, "===== %s =====\n%s\n\n", title, strings.TrimRight(body, "\n"))
	}
	section("ERROR DETAILS", test.ErrorDetails)
	section("STACK TRACE", test.ErrorStackTrace)
	section("STDOUT", test.Stdout)
	section("STDERR", test.Stderr)
	if b.Len() == 0 {
		return "(no failure output was provided by Jenkins for this test)\n"
	}
	return b.String()
}

// sanitize makes a test name safe to use as a single filename segment. The same
// scheme is used by the log, handoff, and notebook files so a test's artifacts
// share a stable basename.
func sanitize(s string) string {
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}
	out := strings.Map(repl, s)
	if len(out) > 180 {
		out = out[:180]
	}
	if out == "" {
		return "test"
	}
	return out
}
