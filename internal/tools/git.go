package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/workspace"
)

const (
	// gitTimeout caps a single git invocation. blame/log on a huge history can
	// be slow; this keeps a pathological repo from hanging the agent.
	gitTimeout = 30 * time.Second
	// gitLogDefaultLimit is the number of commits git_log returns when the model
	// does not specify a limit.
	gitLogDefaultLimit = 10
	// gitLogMaxLimit caps how many commits git_log will return in one call.
	gitLogMaxLimit = 50
	// gitBlameDefaultSpan is how many lines git_blame covers when given a start
	// line but no end line, so a lone start cannot blame an entire huge file.
	gitBlameDefaultSpan = 100
)

// runGit executes a git subcommand with the workspace root as the working
// directory and returns its captured stdout (stderr is folded in on failure).
// A nil error means git exited 0. exitErr is true when git ran but exited
// non-zero (e.g. path not tracked); startErr is true when git could not be run
// at all (not installed, etc.).
func runGit(ctx context.Context, root string, args ...string) (out string, exitErr bool, startErr error) {
	runCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), false, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// git ran but reported an error; surface its stderr to the model.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = stdout.String()
		}
		return msg, true, nil
	}
	return "", false, err
}

// ---------------------------------------------------------------------------
// git_blame
// ---------------------------------------------------------------------------

type gitBlameTool struct{ ws *workspace.Workspace }

func (t *gitBlameTool) Name() string { return "git_blame" }
func (t *gitBlameTool) Description() string {
	return "Show who last changed each line of a file, and in which commit, for a line range. " +
		"Use it to find when a suspicious line was introduced and by which commit/author — invaluable for " +
		"deciding whether a failure is a recent regression. Provide 'start' and 'end' to bound the range; " +
		"a start without an end covers a small window. Workspace-relative path only; output is capped."
}
func (t *gitBlameTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Workspace-relative path to the file to blame.",
			},
			"start": map[string]interface{}{
				"type":        "integer",
				"description": "1-based first line to blame. Omit to blame from the top of the file.",
			},
			"end": map[string]interface{}{
				"type":        "integer",
				"description": "1-based last line to blame. Omit to blame a small window from 'start'.",
			},
		},
		"required": []string{"path"},
	}
}

func (t *gitBlameTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	path, hasPath := strArg(args, "path")
	if !hasPath {
		return fail("git_blame: 'path' is required")
	}
	abs, err := t.ws.Resolve(path)
	if err != nil {
		return fail("git_blame: %v", err)
	}
	rel := t.ws.Rel(abs)

	gitArgs := []string{"blame", "--date=short"}
	start, hasStart := intArg(args, "start")
	end, hasEnd := intArg(args, "end")
	switch {
	case hasStart && hasEnd:
		if start < 1 {
			start = 1
		}
		if end < start {
			return fail("git_blame: 'end' (%d) must be >= 'start' (%d)", end, start)
		}
		gitArgs = append(gitArgs, "-L", fmt.Sprintf("%d,%d", start, end))
	case hasStart:
		if start < 1 {
			start = 1
		}
		gitArgs = append(gitArgs, "-L", fmt.Sprintf("%d,+%d", start, gitBlameDefaultSpan))
	}
	gitArgs = append(gitArgs, "--", rel)

	out, exitErr, startErr := runGit(ctx, t.ws.Root(), gitArgs...)
	if startErr != nil {
		return fail("git_blame: could not run git: %v", startErr)
	}
	if exitErr {
		return ok(map[string]interface{}{
			"path":    rel,
			"message": "git blame did not succeed (the workspace may not be a git repository, or the path may not be tracked): " + capString(out),
		}), nil
	}

	body, truncated := capStringTrunc(out)
	result := map[string]interface{}{
		"path":  rel,
		"blame": body,
	}
	if truncated {
		result["truncated"] = true
		result["note"] = "output truncated; narrow the line range with start/end"
	}
	return ok(result), nil
}

// ---------------------------------------------------------------------------
// git_log
// ---------------------------------------------------------------------------

type gitLogTool struct{ ws *workspace.Workspace }

func (t *gitLogTool) Name() string { return "git_log" }
func (t *gitLogTool) Description() string {
	return "Show recent commits, most recent first, optionally restricted to one file or directory. " +
		"Use it to see what changed recently around the failing code — a recent commit is a prime suspect for a " +
		"regression. Set 'patch' to true to include the diff of each commit (much larger output). " +
		"Workspace-relative path only; output is capped."
}
func (t *gitLogTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Workspace-relative file or directory to restrict the log to. Omit for the whole repository.",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": fmt.Sprintf("Maximum number of commits to return (default %d, max %d).", gitLogDefaultLimit, gitLogMaxLimit),
			},
			"patch": map[string]interface{}{
				"type":        "boolean",
				"description": "Include the diff for each commit. Defaults to false; set true only when you need to see what actually changed.",
			},
		},
		"required": []string{},
	}
}

func (t *gitLogTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	limit, hasLimit := intArg(args, "limit")
	if !hasLimit || limit < 1 {
		limit = gitLogDefaultLimit
	}
	if limit > gitLogMaxLimit {
		limit = gitLogMaxLimit
	}
	patch := boolArg(args, "patch")

	gitArgs := []string{"log", fmt.Sprintf("-n%d", limit), "--date=short"}
	if patch {
		gitArgs = append(gitArgs, "--patch")
	} else {
		gitArgs = append(gitArgs, "--stat", "--pretty=format:%h %ad %an %s")
	}

	var rel string
	if path, hasPath := strArg(args, "path"); hasPath {
		abs, err := t.ws.Resolve(path)
		if err != nil {
			return fail("git_log: %v", err)
		}
		rel = t.ws.Rel(abs)
		gitArgs = append(gitArgs, "--", rel)
	}

	out, exitErr, startErr := runGit(ctx, t.ws.Root(), gitArgs...)
	if startErr != nil {
		return fail("git_log: could not run git: %v", startErr)
	}
	if exitErr {
		return ok(map[string]interface{}{
			"message": "git log did not succeed (the workspace may not be a git repository, or the path may not be tracked): " + capString(out),
		}), nil
	}

	body, truncated := capStringTrunc(out)
	result := map[string]interface{}{
		"limit": limit,
		"log":   body,
	}
	if rel != "" {
		result["path"] = rel
	}
	if truncated {
		result["truncated"] = true
		result["note"] = "output truncated; lower 'limit' or set patch=false"
	}
	return ok(result), nil
}
