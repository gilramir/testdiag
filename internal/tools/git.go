package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/workspace"
)

// Caps and limits for the git tools. Test failures are usually caused by a
// recent change, so blame/log of a suspect file is high-value — but the output
// must stay bounded.
const (
	maxGitBytes    = 256 << 10 // cap raw git output fed back to the model
	gitTimeout     = 30 * time.Second
	defaultLogN    = 15  // commits returned by git_log when unspecified
	maxLogN        = 100 // hard cap on commits returned by git_log
	maxBlameSpan   = 400 // max line span blame will annotate when start/end given
	gitLogPretty   = "%h %ad %an: %s"
	gitLogDateForm = "short"
)

// runGit runs git inside dir with a bounded timeout, returning combined
// stdout+stderr (so error messages from git are visible to the model) and
// whether the output was truncated. The pager is disabled and the terminal
// prompt suppressed so git can never block.
func runGit(ctx context.Context, dir string, args ...string) (out string, truncated bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	full := append([]string{"--no-pager"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	raw, runErr := cmd.CombinedOutput()
	if len(raw) > maxGitBytes {
		raw = raw[:maxGitBytes]
		truncated = true
	}
	return string(raw), truncated, runErr
}

// ---------------------------------------------------------------------------
// git_blame
// ---------------------------------------------------------------------------

type gitBlameTool struct{ ws *workspace.Workspace }

func (t *gitBlameTool) Name() string { return "git_blame" }
func (t *gitBlameTool) Description() string {
	return "Show, for each line of a workspace file, the commit, author and date that last changed it (git blame). Provide start/end to blame only a suspect line range. Use this to find WHO/WHAT/WHEN last touched the failing code — recent churn is a common cause of a newly flaky test."
}
func (t *gitBlameTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":  map[string]interface{}{"type": "string", "description": "Workspace-relative file path to blame."},
			"start": map[string]interface{}{"type": "integer", "description": "First line to blame (1-based). Optional; blames the whole file if omitted."},
			"end":   map[string]interface{}{"type": "integer", "description": "Last line to blame (inclusive). Defaults to 'start'."},
		},
		"required": []string{"path"},
	}
}
func (t *gitBlameTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	path, has := strArg(args, "path")
	if !has {
		return fail("git_blame: 'path' is required")
	}
	rel, err := t.relInRepo(path)
	if err != nil {
		return fail("git_blame: %v", err)
	}

	gitArgs := []string{"blame", "--date=short", "-w"}
	if start, hasStart := intArg(args, "start"); hasStart {
		if start < 1 {
			start = 1
		}
		end, hasEnd := intArg(args, "end")
		if !hasEnd || end < start {
			end = start
		}
		if end-start+1 > maxBlameSpan {
			end = start + maxBlameSpan - 1
		}
		gitArgs = append(gitArgs, "-L", fmt.Sprintf("%d,%d", start, end))
	}
	// `--` ensures the model-supplied path is treated as a pathspec, never a flag.
	gitArgs = append(gitArgs, "--", rel)

	out, truncated, err := runGit(ctx, t.ws.Root(), gitArgs...)
	if err != nil {
		return fail("git_blame: %v: %s", err, strings.TrimSpace(out))
	}
	return ok(map[string]interface{}{
		"path":      rel,
		"blame":     out,
		"truncated": truncated,
	}), nil
}
func (t *gitBlameTool) relInRepo(p string) (string, error) { return relInRepo(t.ws, p) }

// ---------------------------------------------------------------------------
// git_log
// ---------------------------------------------------------------------------

type gitLogTool struct{ ws *workspace.Workspace }

func (t *gitLogTool) Name() string { return "git_log" }
func (t *gitLogTool) Description() string {
	return "Show the recent commit history (hash, date, author, subject) that touched a workspace file. Set patch=true to include each commit's diff. Use this to see WHAT changed in the failing code lately."
}
func (t *gitLogTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":  map[string]interface{}{"type": "string", "description": "Workspace-relative file (or directory) path whose history to show."},
			"limit": map[string]interface{}{"type": "integer", "description": fmt.Sprintf("Max commits to return (default %d, max %d).", defaultLogN, maxLogN)},
			"patch": map[string]interface{}{"type": "boolean", "description": "Include the diff of each commit (default false)."},
		},
		"required": []string{"path"},
	}
}
func (t *gitLogTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	path, has := strArg(args, "path")
	if !has {
		return fail("git_log: 'path' is required")
	}
	rel, err := relInRepo(t.ws, path)
	if err != nil {
		return fail("git_log: %v", err)
	}
	n := defaultLogN
	if v, ok := intArg(args, "limit"); ok && v > 0 {
		n = v
	}
	if n > maxLogN {
		n = maxLogN
	}

	gitArgs := []string{
		"log", "--no-color",
		fmt.Sprintf("-n%d", n),
		"--date=" + gitLogDateForm,
		"--pretty=format:" + gitLogPretty,
	}
	if boolArg(args, "patch") {
		gitArgs = append(gitArgs, "--patch")
	}
	gitArgs = append(gitArgs, "--", rel)

	out, truncated, err := runGit(ctx, t.ws.Root(), gitArgs...)
	if err != nil {
		return fail("git_log: %v: %s", err, strings.TrimSpace(out))
	}
	return ok(map[string]interface{}{
		"path":      rel,
		"log":       out,
		"truncated": truncated,
	}), nil
}

// relInRepo validates a model-supplied path through the workspace jail and
// returns it relative to the workspace root, suitable for passing to git after a
// `--` separator. Jailing happens before git ever sees the path, so a crafted
// path cannot reach outside the checkout or be read as a git option.
func relInRepo(ws *workspace.Workspace, p string) (string, error) {
	abs, err := ws.Resolve(p)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("cannot access %q: %w", p, err)
	}
	return filepath.ToSlash(ws.Rel(abs)), nil
}
