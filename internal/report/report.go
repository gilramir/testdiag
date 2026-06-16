// Package report writes a Markdown root-cause report for a diagnosed test.
package report

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/gilramir/testdiag/internal/pipeline"
)

// localePrinter formats numbers according to the running user's locale,
// detected from LC_NUMERIC, LC_ALL, or LANG (falling back to English).
var localePrinter = func() *message.Printer {
	for _, env := range []string{"LC_NUMERIC", "LC_ALL", "LANG"} {
		if v := os.Getenv(env); v != "" {
			if i := strings.IndexByte(v, '.'); i >= 0 {
				v = v[:i]
			}
			if t, err := language.Parse(v); err == nil {
				return message.NewPrinter(t)
			}
		}
	}
	return message.NewPrinter(language.English)
}()

func commas(n int) string { return localePrinter.Sprintf("%d", n) }

// Write writes a single diagnosis as a Markdown file under outDir and returns
// the path written.
func Write(outDir string, r pipeline.FinalResult) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	name := sanitizeFilename(r.Test.FullName()) + ".md"
	path := filepath.Join(outDir, name)
	if err := os.WriteFile(path, []byte(render(r)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func render(r pipeline.FinalResult) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Test failure root cause: %s\n\n", r.Test.FullName())

	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Test | `%s` |\n", r.Test.FullName())
	if r.Test.Status != "" {
		fmt.Fprintf(&b, "| Status | %s |\n", r.Test.Status)
	}
	if r.LogPath != "" {
		fmt.Fprintf(&b, "| Saved log | `%s` |\n", r.LogPath)
	}
	if r.Test.ReportURL != "" {
		fmt.Fprintf(&b, "| Jenkins report | %s |\n", r.Test.ReportURL)
	}
	fmt.Fprintf(&b, "| Hypotheses | %d |\n", len(r.Hypotheses))
	approved, failed := countOutcomes(r.DeepInspects)
	fmt.Fprintf(&b, "| Deep inspections | %d approved, %d failed |\n", approved, failed)
	fmt.Fprintf(&b, "| Analyzed | %s |\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "| Duration | %s |\n", r.Duration.Round(time.Millisecond))
	if !r.TotalUsage.IsZero() {
		fmt.Fprintf(&b, "| Tokens | %s total (%s prompt + %s completion) |\n",
			commas(r.TotalUsage.Total), commas(r.TotalUsage.Prompt), commas(r.TotalUsage.Completion))
	}
	b.WriteString("\n---\n\n")

	body := strings.TrimSpace(r.Summary)
	if body == "" {
		body = "_The SUMMARIZE stage produced no output._"
	}
	b.WriteString(body)
	b.WriteString("\n")

	// Token usage breakdown by stage.
	if hasNonZeroUsage(r.StageUsages) {
		b.WriteString("\n---\n\n## Token usage by stage\n\n")
		b.WriteString("| Stage | Prompt | Completion | Total |\n")
		b.WriteString("|---|---:|---:|---:|\n")
		for _, su := range r.StageUsages {
			if su.Usage.IsZero() {
				continue
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
				su.Name, commas(su.Usage.Prompt), commas(su.Usage.Completion), commas(su.Usage.Total))
		}
		fmt.Fprintf(&b, "| **TOTAL** | **%s** | **%s** | **%s** |\n",
			commas(r.TotalUsage.Prompt), commas(r.TotalUsage.Completion), commas(r.TotalUsage.Total))
		b.WriteString("\n")
	}

	// Appendix: per-hypothesis DEEPINSPECT results for traceability.
	if len(r.DeepInspects) > 0 {
		b.WriteString("\n---\n\n## Appendix: per-hypothesis investigation results\n\n")
		for _, o := range r.DeepInspects {
			fmt.Fprintf(&b, "### Hypothesis %d: %s\n\n", o.Hypothesis.Index, o.Hypothesis.Title)
			if o.Failed {
				fmt.Fprintf(&b, "_DEEPINSPECT failed: %s_\n\n", o.FailReason)
			} else {
				fmt.Fprintf(&b, "_DEEPINSPECT: %s_\n\n", approvalLabel(o.FeedbackApproved))
				if len(o.ToolsCalled) > 0 {
					fmt.Fprintf(&b, "_Tools used: %s_\n\n", strings.Join(o.ToolsCalled, ", "))
				}
				content := strings.TrimSpace(o.Content)
				if content != "" {
					b.WriteString("<details><summary>Full analysis</summary>\n\n")
					b.WriteString(content)
					b.WriteString("\n\n</details>\n\n")
				}
			}
		}
	}

	return b.String()
}

func hasNonZeroUsage(stages []pipeline.StageUsage) bool {
	for _, s := range stages {
		if !s.Usage.IsZero() {
			return true
		}
	}
	return false
}

func countOutcomes(outcomes []pipeline.DeepInspectOutcome) (approved, failed int) {
	for _, o := range outcomes {
		if o.Failed {
			failed++
		} else {
			approved++
		}
	}
	return
}

func approvalLabel(approved bool) string {
	if approved {
		return "FEEDBACK APPROVED"
	}
	return "feedback not run"
}

func sanitizeFilename(s string) string {
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
