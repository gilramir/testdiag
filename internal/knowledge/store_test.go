package knowledge

import (
	"strings"
	"testing"
)

func TestCoalesceLineIntervals(t *testing.T) {
	s := New(0)
	// Read 10-20, 20-30, 50-60 (out of order, overlapping at 20).
	s.AddLines("a.go", 50, mkLines(50, 60))
	s.AddLines("a.go", 10, mkLines(10, 20))
	s.AddLines("a.go", 20, mkLines(20, 30))

	out := s.Render()
	// Expect two runs: 10-30 and 50-60.
	if !strings.Contains(out, "lines 10-30:") {
		t.Errorf("expected coalesced run 10-30, got:\n%s", out)
	}
	if !strings.Contains(out, "lines 50-60:") {
		t.Errorf("expected run 50-60, got:\n%s", out)
	}
	if strings.Contains(out, "lines 10-20:") || strings.Contains(out, "lines 20-30:") {
		t.Errorf("intervals not coalesced:\n%s", out)
	}
}

func TestDedupAndChangeReporting(t *testing.T) {
	s := New(0)
	if !s.AddLines("a.go", 1, []string{"x", "y"}) {
		t.Error("first add should report a change")
	}
	if s.AddLines("a.go", 1, []string{"x", "y"}) {
		t.Error("re-adding identical lines should report no change")
	}
	if !s.AddLines("a.go", 2, []string{"y", "z"}) {
		t.Error("adding a new line (3) should report a change")
	}

	if !s.AddSearch("search_repo", `"lock"`, []string{"a.go:1"}) {
		t.Error("first search should report a change")
	}
	if s.AddSearch("search_repo", `"lock"`, []string{"a.go:1"}) {
		t.Error("repeated identical search should report no change")
	}
	if !s.AddSearch("search_repo", `"lock"`, []string{"b.go:9"}) {
		t.Error("search with a new hit should report a change")
	}
}

func TestNotFoundMarker(t *testing.T) {
	s := New(0)
	s.MarkNotFound("ghost.go")
	out := s.Render()
	if !strings.Contains(out, "path: ghost.go") || !strings.Contains(out, "not_found: true") {
		t.Errorf("expected not-found marker, got:\n%s", out)
	}
	// A later successful read clears the marker.
	if !s.AddLines("ghost.go", 1, []string{"real"}) {
		t.Error("reading a previously-not-found file should report a change")
	}
	out = s.Render()
	if strings.Contains(out, "not_found: true") {
		t.Errorf("not-found marker should be cleared after a read:\n%s", out)
	}
}

func TestSearchNoResults(t *testing.T) {
	s := New(0)
	s.SetSearchNote("find_files", "*.race", "no matches")
	out := s.Render()
	if !strings.Contains(out, "tool: find_files") || !strings.Contains(out, "params: *.race") || !strings.Contains(out, "no matches") {
		t.Errorf("expected no-match note, got:\n%s", out)
	}
}

func TestEvictionElidesThenDrops(t *testing.T) {
	full := New(0)
	addBig(full)
	fullOut := full.Render()

	// Budget tight enough to force eliding line text but not necessarily drops.
	capped := New(len(fullOut) / 2)
	addBig(capped)
	out := capped.Render()
	if len(out) > len(fullOut)/2 {
		t.Errorf("rendered output %d exceeds budget %d:\n%s", len(out), len(fullOut)/2, out)
	}
	if !strings.Contains(out, "content_elided") && !strings.Contains(out, "path:") {
		t.Errorf("expected elision or partial content under budget, got:\n%s", out)
	}

	// Very tight budget should still produce something bounded.
	tiny := New(120)
	addBig(tiny)
	tout := tiny.Render()
	if len(tout) > 400 { // eviction is greedy/approximate; just ensure it shrank a lot
		t.Errorf("tiny budget did not shrink output enough: %d chars", len(tout))
	}
}

func TestEvictionKeepsRecentFacts(t *testing.T) {
	s := New(0)
	s.AddLines("old.go", 1, mkLines(1, 40))    // created first
	s.AddLines("recent.go", 1, mkLines(1, 40)) // created later
	full := s.Render()

	capped := New(len(full) - len(full)/3)
	capped.AddLines("old.go", 1, mkLines(1, 40))
	capped.AddLines("recent.go", 1, mkLines(1, 40))
	out := capped.Render()

	// old.go is least-recently-touched, so its content should be elided first
	// while recent.go keeps real content.
	oldElided := strings.Contains(out, "path: old.go") &&
		strings.Contains(afterEntry(out, "path: old.go"), "content_elided")
	if !oldElided {
		t.Errorf("expected old.go to be elided first:\n%s", out)
	}
}

func TestEvictionByCreationNotModification(t *testing.T) {
	// old.go is created first but then written again (high touched seq).
	// new.go is created later.
	// Under a tight budget, old.go should still be evicted first because
	// eviction is ordered by creation time, not last-modification time.
	s := New(0)
	s.AddLines("old.go", 1, mkLines(1, 40))
	s.AddLines("new.go", 1, mkLines(1, 40))
	s.AddLines("old.go", 41, mkLines(41, 80)) // re-touch old.go after new.go exists
	full := s.Render()

	capped := New(len(full) - len(full)/3)
	capped.AddLines("old.go", 1, mkLines(1, 40))
	capped.AddLines("new.go", 1, mkLines(1, 40))
	capped.AddLines("old.go", 41, mkLines(41, 80))
	out := capped.Render()

	oldElided := strings.Contains(out, "path: old.go") &&
		strings.Contains(afterEntry(out, "path: old.go"), "content_elided")
	if !oldElided {
		t.Errorf("expected old.go (oldest created) to be elided first despite recent write:\n%s", out)
	}
	newKept := strings.Contains(out, "path: new.go") &&
		!strings.Contains(afterEntry(out, "path: new.go"), "content_elided")
	if !newKept {
		t.Errorf("expected new.go (newer created) to keep its content:\n%s", out)
	}
}

func TestJSONDump(t *testing.T) {
	s := New(0)
	s.AddLines("a.go", 10, []string{"x", "y"})
	s.MarkNotFound("b.go")
	s.AddSearch("search_repo", `"q"`, []string{"a.go:10"})
	b, err := s.JSON()
	if err != nil {
		t.Fatal(err)
	}
	j := string(b)
	for _, want := range []string{"a.go", "b.go", "not_found", "search_repo", "10-11"} {
		if !strings.Contains(j, want) {
			t.Errorf("JSON missing %q:\n%s", want, j)
		}
	}
}

// --- helpers ---

func mkLines(start, end int) []string {
	var out []string
	for i := start; i <= end; i++ {
		out = append(out, "line content here")
	}
	return out
}

func addBig(s *Store) {
	for _, p := range []string{"a.go", "b.go", "c.go"} {
		s.AddLines(p, 1, mkLines(1, 30))
	}
	s.AddSearch("search_repo", `"lock"`, []string{"a.go:1", "b.go:2", "c.go:3"})
}

// afterEntry returns the text following the first occurrence of marker up to
// the next TOON file entry ("  - path:") or end of string.
func afterEntry(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	if j := strings.Index(rest, "  - path:"); j >= 0 {
		return rest[:j]
	}
	return rest
}
