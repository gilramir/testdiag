package toolproto

import "testing"

func TestParseCanonical(t *testing.T) {
	calls := Parse(`I'll look. TOOL_CALL{"name":"read_file","args":{"path":"main.go"}} done.`)
	if len(calls) != 1 || calls[0].Name != "read_file" || calls[0].Args["path"] != "main.go" {
		t.Fatalf("got %#v", calls)
	}
}

func TestParseReason(t *testing.T) {
	calls := Parse(`TOOL_CALL{"name":"grep","reason":"locate the mutex guard","args":{"path":"lock.go","pattern":"Mutex"}}`)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %#v", len(calls), calls)
	}
	if calls[0].Reason != "locate the mutex guard" {
		t.Errorf("reason = %q, want %q", calls[0].Reason, "locate the mutex guard")
	}
	if calls[0].Args["path"] != "lock.go" {
		t.Errorf("args.path = %v", calls[0].Args["path"])
	}
}

func TestParseNoReason(t *testing.T) {
	// reason is optional; calls without it should still parse cleanly.
	calls := Parse(`TOOL_CALL{"name":"read_file","args":{"path":"main.go"}}`)
	if len(calls) != 1 || calls[0].Reason != "" {
		t.Fatalf("expected 1 call with empty reason, got %#v", calls)
	}
}

func TestParseNativeNormalized(t *testing.T) {
	// Gemma-style native syntax should be normalized then parsed.
	calls := Parse("```tool_code\nsearch_repo(query=\"lock\")\n```")
	if len(calls) != 1 || calls[0].Name != "search_repo" || calls[0].Args["query"] != "lock" {
		t.Fatalf("got %#v", calls)
	}
}

func TestParseMultiple(t *testing.T) {
	calls := Parse(`TOOL_CALL{"name":"a","args":{}} TOOL_CALL{"name":"b","args":{"x":1}}`)
	if len(calls) != 2 || calls[0].Name != "a" || calls[1].Name != "b" {
		t.Fatalf("got %#v", calls)
	}
}

func TestParseNoCall(t *testing.T) {
	if calls := Parse("Here is my final answer: the bug is a race."); calls != nil {
		t.Fatalf("expected nil, got %#v", calls)
	}
}
