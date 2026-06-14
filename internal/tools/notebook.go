package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"


	"github.com/gilbertr/testdiag/internal/workspace"
)

// Caps for the notebook so a runaway agent can't fill the context window by
// writing — or re-reading — an unbounded scratchpad.
const (
	maxNoteBytes     = 8 << 10  // largest single note appended
	maxNotebookBytes = 64 << 10 // most we hand back on a read (tail-most kept)
)

// notebookPath is the workspace-relative path of the CURRENT test's notebook.
// It is package-global because the tools are shared, stateless singletons;
// diagnose sets it (sequentially, one test at a time) via SetNotebookPath before
// each run, the same way it resets the loop guard.
var (
	notebookMu   sync.Mutex
	notebookPath string
)

// SetNotebookPath points the notebook tool at a test's notes file (a
// workspace-relative path such as ".testdiag/notes/Foo.md"). Pass "" to disable
// note-taking; the tool then reports that it is unavailable.
func SetNotebookPath(rel string) {
	notebookMu.Lock()
	notebookPath = rel
	notebookMu.Unlock()
}

func currentNotebookPath() string {
	notebookMu.Lock()
	defer notebookMu.Unlock()
	return notebookPath
}

// ---------------------------------------------------------------------------
// notebook
// ---------------------------------------------------------------------------

type notebookTool struct{ ws *workspace.Workspace }

func (t *notebookTool) Name() string { return "notebook" }
func (t *notebookTool) Description() string {
	return "Your private Markdown notebook for THIS test — a scratchpad that persists across tool calls so you don't lose the thread. " +
		"Use action='append' with a short 'note' to record what you are looking for and WHY before you dig in: your current hypothesis, " +
		"what you have already ruled out, and the next thing to check. Use action='read' to re-read everything you have written so far and " +
		"refresh your memory when the investigation gets long. Only you can see it; keep entries short and write conclusions, not raw dumps."
}
func (t *notebookTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"append", "read"},
				"description": "'append' to add a note, 'read' to read the whole notebook back.",
			},
			"note": map[string]interface{}{
				"type":        "string",
				"description": "The Markdown note to append (required when action is 'append'). Say what you are looking for and why.",
			},
		},
		"required": []string{"action"},
	}
}

// loopExempt marks the notebook so the loop guard never intercepts it: re-reads
// legitimately return more each time the agent appends, and that is the whole
// point of the tool, so repeated calls are not a stuck loop.
func (t *notebookTool) loopExempt() {}

func (t *notebookTool) Execute(ctx context.Context, args map[string]interface{}) (*Result, error) {
	rel := currentNotebookPath()
	if rel == "" {
		return fail("notebook: note-taking is not enabled for this run")
	}
	abs, err := t.ws.Resolve(rel)
	if err != nil {
		return fail("notebook: %v", err)
	}

	action, _ := strArg(args, "action")
	switch action {
	case "append":
		note, hasNote := strArg(args, "note")
		if !hasNote {
			return fail("notebook: 'note' is required when action is 'append'")
		}
		if len(note) > maxNoteBytes {
			return fail("notebook: note is %d bytes, exceeding the %d-byte limit; write a shorter, summarized note", len(note), maxNoteBytes)
		}
		if err := appendNote(abs, note); err != nil {
			return fail("notebook: %v", err)
		}
		return ok(map[string]interface{}{
			"action":   "append",
			"appended": true,
			"message":  "Note saved. Read it back later with action='read' to refresh your memory.",
		}), nil

	case "read", "":
		content, truncated, err := readNotebook(abs)
		if err != nil {
			return fail("notebook: %v", err)
		}
		return ok(map[string]interface{}{
			"action":    "read",
			"content":   content,
			"empty":     strings.TrimSpace(content) == "",
			"truncated": truncated,
		}), nil

	default:
		return fail("notebook: unknown action %q (use 'append' or 'read')", action)
	}
}

// appendNote appends one note to the notebook, creating the file (and its parent
// directory) if needed. Each note is separated by a blank line so successive
// entries stay readable as Markdown.
func appendNote(abs, note string) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.TrimRight(note, "\n") + "\n\n")
	return err
}

// readNotebook returns the notebook's contents, keeping the most recent
// maxNotebookBytes (the tail) when it is large, since the latest notes are the
// ones worth refreshing on. A missing notebook reads as empty, not an error.
func readNotebook(abs string) (content string, truncated bool, err error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if len(data) > maxNotebookBytes {
		data = data[len(data)-maxNotebookBytes:]
		truncated = true
	}
	return string(data), truncated, nil
}
