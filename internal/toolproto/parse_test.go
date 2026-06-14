package toolproto

import "testing"

func TestParseCanonical(t *testing.T) {
	calls := Parse(`I'll look. TOOL_CALL{"name":"read_file","args":{"path":"main.go"}} done.`)
	if len(calls) != 1 || calls[0].Name != "read_file" || calls[0].Args["path"] != "main.go" {
		t.Fatalf("got %#v", calls)
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
