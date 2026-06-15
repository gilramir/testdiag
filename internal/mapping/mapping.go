// Package mapping translates a failed Jenkins test into the path of its source
// file within the local workspace by running a user-supplied executable.
package mapping

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gilramir/testdiag/internal/jenkins"
)

// Result is the outcome of mapping a test to its source location.
type Result struct {
	// SourceFile is the workspace-relative path to the test's source file
	// (e.g. "src/main/java/com/acme/Foo.java" or "pkg/foo/foo_test.go").
	// Empty when the mapping is unknown — diagnosis still proceeds and the
	// agent can locate the file itself via the directory/grep tools.
	SourceFile string

	// Notes is optional human-readable context about how the path was derived;
	// it is included in the DEEPINSPECT prompt to help the agent.
	Notes string
}

// MapTestToSource runs the mapper executable (if configured) with the test's
// full name as the sole argument and returns the source file path it prints to
// stdout. The subprocess runs with workspaceRoot as its working directory so
// the mapper can resolve relative paths or consult workspace files.
//
// When mapperPath is empty the function returns an empty Result and no error —
// the agent will locate the file itself. When the mapper exits non-zero or
// times out an error is returned; callers should log a warning and continue
// with an empty Result rather than aborting the diagnosis.
func MapTestToSource(mapperPath, workspaceRoot string, test jenkins.FailedTest) (Result, error) {
	if mapperPath == "" {
		return Result{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !filepath.IsAbs(mapperPath) {
		mapperPath = filepath.Join(workspaceRoot, mapperPath)
	}
	cmd := exec.CommandContext(ctx, mapperPath, test.FullName())
	cmd.Dir = workspaceRoot
	out, err := cmd.Output() // stderr is left uncaptured so the mapper can log there
	if err != nil {
		return Result{}, fmt.Errorf("mapper %q exited with error: %w", mapperPath, err)
	}

	file := strings.TrimSpace(string(out))
	if file == "" {
		return Result{}, nil
	}
	return Result{SourceFile: file}, nil
}
