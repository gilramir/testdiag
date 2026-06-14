// Package knowledge accumulates the facts a tool-using inspection agent has
// gathered into a single deduplicated structure that is re-rendered into the
// LLM context on every turn. It replaces AgenticGoKit's single-step relay loop,
// which discarded every tool result older than one turn and so left the agent
// with no working memory.
//
// Conceptually the store is a tree of facts. Two kinds are tracked:
//
//   - File records — per-file content gathered by read_file / read_lines (and
//     enriched by grep matches), stored as a sparse line->text map so repeated
//     or overlapping reads coalesce into non-overlapping intervals (a read of
//     10-20 then 20-30 becomes a single "lines 10-30"); plus explicit
//     "not found" markers so the agent does not re-request a missing path.
//   - Search records — per-query results from search_repo / find_files /
//     file_exists / git tools, keyed by the canonical (tool, params) pair so a
//     repeated query is a no-op merge rather than duplicated noise.
//
// Render produces the Markdown the LLM sees; JSON produces a faithful debug
// dump. When the rendered Markdown would exceed the configured character
// budget, least-recently-referenced facts are evicted: file line-text is elided
// first (keeping the interval index), then whole records are dropped.
package knowledge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Store is the accumulated knowledge for one inspection run. It is not safe for
// concurrent use; a single agent loop owns it.
type Store struct {
	maxChars  int // render budget in characters; 0 = unlimited
	seq       int // monotonic counter for least-recently-referenced eviction
	files     []*fileRecord
	fileIdx   map[string]*fileRecord
	searches  []*searchRecord
	searchIdx map[string]*searchRecord
}

type fileRecord struct {
	Path     string
	NotFound bool
	Note     string         // optional, e.g. "120 lines" or "directory"
	Lines    map[int]string // line number -> text; nil until content is read
	touched  int
}

type searchRecord struct {
	Tool    string
	Params  string // human-readable canonical params, e.g. `"acquireLock"`
	Results []string
	Note    string // optional, e.g. "no matches"
	touched int
}

// New returns an empty store. maxChars caps the size of the rendered Markdown;
// pass 0 for no cap.
func New(maxChars int) *Store {
	return &Store{
		maxChars:  maxChars,
		fileIdx:   map[string]*fileRecord{},
		searchIdx: map[string]*searchRecord{},
	}
}

func (s *Store) bump() int { s.seq++; return s.seq }

func (s *Store) file(path string) *fileRecord {
	if r, ok := s.fileIdx[path]; ok {
		return r
	}
	r := &fileRecord{Path: path}
	s.fileIdx[path] = r
	s.files = append(s.files, r)
	return r
}

// AddLines records that lines [start, start+len(lines)-1] of path hold the given
// text. Overlapping or adjacent reads coalesce automatically. Returns true if
// any line was new (or a prior not-found marker was cleared).
func (s *Store) AddLines(path string, start int, lines []string) bool {
	r := s.file(path)
	changed := r.NotFound // clearing a not-found marker counts as a change
	r.NotFound = false
	if r.Lines == nil {
		r.Lines = map[int]string{}
	}
	for i, ln := range lines {
		n := start + i
		if _, ok := r.Lines[n]; !ok {
			changed = true
		}
		r.Lines[n] = ln
	}
	r.touched = s.bump()
	return changed
}

// AddWholeFile records the full content of path (line numbering starts at 1).
func (s *Store) AddWholeFile(path, content string) bool {
	return s.AddLines(path, 1, splitLines(content))
}

// AddMatch records a single grep/search hit at path:line with its text, both as
// a search result line and as known content for that file.
func (s *Store) AddMatch(path string, line int, text string) {
	s.AddLines(path, line, []string{text})
}

// MarkNotFound records that path does not exist, so the agent stops asking.
func (s *Store) MarkNotFound(path string) bool {
	r := s.file(path)
	changed := !r.NotFound
	r.NotFound = true
	r.Lines = nil
	r.touched = s.bump()
	return changed
}

// SetFileNote attaches a short annotation to a file record (e.g. its length).
func (s *Store) SetFileNote(path, note string) {
	r := s.file(path)
	r.Note = note
	r.touched = s.bump()
}

// AddSearch records the results of a query-style tool (search_repo, find_files,
// file_exists, git_*). params is a human-readable rendering of the call's
// arguments; it is also the dedup key together with tool. A repeated query
// merges new results in rather than duplicating the record. Returns true if the
// record was new or gained results.
func (s *Store) AddSearch(tool, params string, results []string) bool {
	key := tool + "\x00" + params
	r, ok := s.searchIdx[key]
	if !ok {
		r = &searchRecord{Tool: tool, Params: params}
		s.searchIdx[key] = r
		s.searches = append(s.searches, r)
	}
	changed := !ok
	seen := map[string]bool{}
	for _, x := range r.Results {
		seen[x] = true
	}
	for _, x := range results {
		if !seen[x] {
			r.Results = append(r.Results, x)
			seen[x] = true
			changed = true
		}
	}
	r.touched = s.bump()
	return changed
}

// SetSearchNote annotates the most recently identified search record (e.g. "no
// matches"). It is keyed the same way as AddSearch.
func (s *Store) SetSearchNote(tool, params, note string) {
	key := tool + "\x00" + params
	r, ok := s.searchIdx[key]
	if !ok {
		r = &searchRecord{Tool: tool, Params: params}
		s.searchIdx[key] = r
		s.searches = append(s.searches, r)
	}
	r.Note = note
	r.touched = s.bump()
}

// Empty reports whether nothing has been recorded yet.
func (s *Store) Empty() bool { return len(s.files) == 0 && len(s.searches) == 0 }

func splitLines(content string) []string {
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// contiguousRuns groups a set of line numbers into [start,end] inclusive runs.
func contiguousRuns(nums []int) [][2]int {
	sort.Ints(nums)
	var runs [][2]int
	for i := 0; i < len(nums); i++ {
		start, end := nums[i], nums[i]
		for i+1 < len(nums) && nums[i+1] == end+1 {
			i++
			end = nums[i]
		}
		runs = append(runs, [2]int{start, end})
	}
	return runs
}

func rangeLabel(runs [][2]int) string {
	parts := make([]string, len(runs))
	for i, r := range runs {
		if r[0] == r[1] {
			parts[i] = fmt.Sprintf("%d", r[0])
		} else {
			parts[i] = fmt.Sprintf("%d-%d", r[0], r[1])
		}
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// Rendering with cap + evict
// ---------------------------------------------------------------------------

type evictState struct {
	elided  map[string]bool // file paths whose line text is dropped
	dropped map[string]bool // record keys ("F:"+path / "S:"+tool\x00params)
}

func newEvictState() *evictState {
	return &evictState{elided: map[string]bool{}, dropped: map[string]bool{}}
}

// Render returns the Markdown view, evicting least-recently-referenced facts to
// stay within the character budget.
func (s *Store) Render() string {
	st := newEvictState()
	out := s.render(st)
	if s.maxChars <= 0 || len(out) <= s.maxChars {
		return out
	}

	// Pass 1: elide file line-text, least-recently-touched first.
	for _, f := range s.byTouchedFiles() {
		if len(f.Lines) == 0 {
			continue
		}
		st.elided[f.Path] = true
		if out = s.render(st); len(out) <= s.maxChars {
			return out
		}
	}

	// Pass 2: drop whole records, least-recently-touched first, across both
	// kinds, until we fit (or nothing remains).
	for _, key := range s.byTouchedAll() {
		st.dropped[key] = true
		if out = s.render(st); len(out) <= s.maxChars {
			return out
		}
	}
	return out
}

func (s *Store) render(st *evictState) string {
	var b strings.Builder

	var fileBlocks []string
	for _, f := range s.files {
		if st.dropped["F:"+f.Path] {
			continue
		}
		fileBlocks = append(fileBlocks, s.renderFile(f, st.elided[f.Path]))
	}
	if len(fileBlocks) > 0 {
		b.WriteString("## Files examined\n\n")
		b.WriteString(strings.Join(fileBlocks, "\n"))
		b.WriteString("\n")
	}

	var searchBlocks []string
	for _, r := range s.searches {
		if st.dropped["S:"+r.Tool+"\x00"+r.Params] {
			continue
		}
		searchBlocks = append(searchBlocks, s.renderSearch(r))
	}
	if len(searchBlocks) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## Searches and lookups\n\n")
		b.WriteString(strings.Join(searchBlocks, "\n"))
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

func (s *Store) renderFile(f *fileRecord, elide bool) string {
	var b strings.Builder
	header := "### " + f.Path
	if f.Note != "" {
		header += " (" + f.Note + ")"
	}
	b.WriteString(header + "\n")

	if f.NotFound {
		b.WriteString("- NOT FOUND\n")
		return b.String()
	}
	if len(f.Lines) == 0 {
		b.WriteString("- (referenced; no content read yet)\n")
		return b.String()
	}

	nums := make([]int, 0, len(f.Lines))
	for n := range f.Lines {
		nums = append(nums, n)
	}
	runs := contiguousRuns(nums)

	if elide {
		fmt.Fprintf(&b, "- known lines %s (content elided to save space)\n", rangeLabel(runs))
		return b.String()
	}

	for _, run := range runs {
		fmt.Fprintf(&b, "- lines %s:\n", rangeLabel([][2]int{run}))
		for n := run[0]; n <= run[1]; n++ {
			fmt.Fprintf(&b, "    %d  %s\n", n, f.Lines[n])
		}
	}
	return b.String()
}

func (s *Store) renderSearch(r *searchRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s %s\n", r.Tool, r.Params)
	if len(r.Results) == 0 {
		note := r.Note
		if note == "" {
			note = "no results"
		}
		fmt.Fprintf(&b, "- %s\n", note)
		return b.String()
	}
	if r.Note != "" {
		fmt.Fprintf(&b, "- %s\n", r.Note)
	}
	for _, res := range r.Results {
		fmt.Fprintf(&b, "- %s\n", res)
	}
	return b.String()
}

// byTouchedFiles returns file records sorted least-recently-touched first.
func (s *Store) byTouchedFiles() []*fileRecord {
	out := append([]*fileRecord(nil), s.files...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].touched < out[j].touched })
	return out
}

// byTouchedAll returns all record keys sorted least-recently-touched first.
func (s *Store) byTouchedAll() []string {
	type item struct {
		key     string
		touched int
	}
	var items []item
	for _, f := range s.files {
		items = append(items, item{"F:" + f.Path, f.touched})
	}
	for _, r := range s.searches {
		items = append(items, item{"S:" + r.Tool + "\x00" + r.Params, r.touched})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].touched < items[j].touched })
	keys := make([]string, len(items))
	for i, it := range items {
		keys[i] = it.key
	}
	return keys
}

// ---------------------------------------------------------------------------
// JSON debug dump
// ---------------------------------------------------------------------------

// JSON returns a stable, pretty-printed dump of the full store (no eviction),
// for writing to disk as a debugging artifact.
func (s *Store) JSON() ([]byte, error) {
	type fileOut struct {
		Path     string   `json:"path"`
		NotFound bool     `json:"not_found,omitempty"`
		Note     string   `json:"note,omitempty"`
		Ranges   string   `json:"known_lines,omitempty"`
		Lines    [][2]any `json:"lines,omitempty"` // [lineNumber, text]
	}
	type searchOut struct {
		Tool    string   `json:"tool"`
		Params  string   `json:"params"`
		Note    string   `json:"note,omitempty"`
		Results []string `json:"results,omitempty"`
	}
	type out struct {
		Files    []fileOut   `json:"files,omitempty"`
		Searches []searchOut `json:"searches,omitempty"`
	}

	var o out
	for _, f := range s.files {
		fo := fileOut{Path: f.Path, NotFound: f.NotFound, Note: f.Note}
		if len(f.Lines) > 0 {
			nums := make([]int, 0, len(f.Lines))
			for n := range f.Lines {
				nums = append(nums, n)
			}
			fo.Ranges = rangeLabel(contiguousRuns(nums))
			sort.Ints(nums)
			for _, n := range nums {
				fo.Lines = append(fo.Lines, [2]any{n, f.Lines[n]})
			}
		}
		o.Files = append(o.Files, fo)
	}
	for _, r := range s.searches {
		o.Searches = append(o.Searches, searchOut{Tool: r.Tool, Params: r.Params, Note: r.Note, Results: r.Results})
	}
	return json.MarshalIndent(o, "", "  ")
}
