package tools

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ToolCall records one tool invocation: the name, arguments, and a compact
// summary of the response (no full content strings).
type ToolCall struct {
	Name          string
	Args          map[string]interface{}
	ResultSummary string // compact description of the response fields
	Failed        bool
}

var (
	toolLogMu    sync.Mutex
	toolLogCalls []ToolCall
)

// ResetToolLog clears the accumulated tool call log. Call this at the start of
// each hypothesis run (before the agent is invoked) so the log is scoped to
// one hypothesis's worth of tool activity.
func ResetToolLog() {
	toolLogMu.Lock()
	toolLogCalls = toolLogCalls[:0]
	toolLogMu.Unlock()
}

// CollectToolLog returns all tool calls recorded since the last ResetToolLog
// and clears the log.
func CollectToolLog() []ToolCall {
	toolLogMu.Lock()
	defer toolLogMu.Unlock()
	out := make([]ToolCall, len(toolLogCalls))
	copy(out, toolLogCalls)
	toolLogCalls = toolLogCalls[:0]
	return out
}

// appendToolCall records one completed tool invocation.
func appendToolCall(name string, args map[string]interface{}, content interface{}, failed bool) {
	var summary string
	if failed {
		summary = fmt.Sprintf("%v", content)
	} else {
		summary = summarizeValue(content)
	}
	toolLogMu.Lock()
	toolLogCalls = append(toolLogCalls, ToolCall{
		Name:          name,
		Args:          args,
		ResultSummary: summary,
		Failed:        failed,
	})
	toolLogMu.Unlock()
}

// FormatToolLog renders a slice of ToolCall records as a Markdown document
// suitable for inclusion in a LESSONS prompt.
func FormatToolLog(calls []ToolCall) string {
	if len(calls) == 0 {
		return "_No tool calls recorded._\n"
	}
	var b strings.Builder
	for i, c := range calls {
		fmt.Fprintf(&b, "## Call %d: %s\n\n", i+1, c.Name)
		if len(c.Args) > 0 {
			keys := make([]string, 0, len(c.Args))
			for k := range c.Args {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s=%s", k, summarizeValue(c.Args[k])))
			}
			fmt.Fprintf(&b, "**Args:** %s\n\n", strings.Join(parts, "  "))
		}
		if c.Failed {
			fmt.Fprintf(&b, "**Result:** FAILED — %s\n\n", c.ResultSummary)
		} else {
			fmt.Fprintf(&b, "**Result:** %s\n\n", c.ResultSummary)
		}
	}
	return b.String()
}

// summarizeValue produces a compact, single-line description of a tool result
// value without including full content strings. Short strings (≤ 120 chars)
// are shown quoted; long strings are replaced with a char+line count. Lists
// show only their item count. Maps recurse one level.
func summarizeValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		if len(val) <= 120 {
			return fmt.Sprintf("%q", val)
		}
		lines := strings.Count(val, "\n")
		if len(val) > 0 && !strings.HasSuffix(val, "\n") {
			lines++
		}
		return fmt.Sprintf("(%d chars, %d lines)", len(val), lines)
	case []interface{}:
		return fmt.Sprintf("%d items", len(val))
	case []string:
		return fmt.Sprintf("%d items", len(val))
	case map[string]interface{}:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+summarizeValue(val[k]))
		}
		return strings.Join(parts, "; ")
	case bool:
		return fmt.Sprintf("%v", val)
	case float64:
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	case int:
		return fmt.Sprintf("%d", val)
	case nil:
		return "null"
	default:
		return fmt.Sprintf("(%T)", val)
	}
}
