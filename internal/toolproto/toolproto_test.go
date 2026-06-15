package toolproto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// parseRendered extracts calls from normalized output the same way the tool loop
// does: split on the TOOL_CALL marker and decode the balanced JSON that follows.
func parseRendered(t *testing.T, content string) []Call {
	t.Helper()
	var calls []Call
	parts := strings.Split(content, "TOOL_CALL")
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		end := matchBracket(p, strings.IndexByte(p, '{'), '{', '}')
		if end < 0 {
			t.Fatalf("rendered TOOL_CALL has no balanced object: %q", p)
		}
		var decoded struct {
			Name string                 `json:"name"`
			Args map[string]interface{} `json:"args"`
		}
		if err := json.Unmarshal([]byte(p[:end+1]), &decoded); err != nil {
			t.Fatalf("decoding rendered TOOL_CALL %q: %v", p[:end+1], err)
		}
		calls = append(calls, Call{Name: decoded.Name, Args: decoded.Args})
	}
	return calls
}

func assertCalls(t *testing.T, content string, want []Call) {
	t.Helper()
	got := parseRendered(t, Normalize(content))
	if len(got) != len(want) {
		t.Fatalf("got %d calls, want %d\nnormalized: %s", len(got), len(want), Normalize(content))
	}
	for i := range want {
		if got[i].Name != want[i].Name {
			t.Errorf("call %d name = %q, want %q", i, got[i].Name, want[i].Name)
		}
		if !reflect.DeepEqual(got[i].Args, want[i].Args) {
			t.Errorf("call %d args = %#v, want %#v", i, got[i].Args, want[i].Args)
		}
	}
}

func TestHarmony(t *testing.T) {
	content := "<|channel|>commentary to=functions.read_file <|constrain|>json" +
		`<|message|>{"path":"main.go"}<|call|>`
	assertCalls(t, content, []Call{
		{Name: "read_file", Args: map[string]interface{}{"path": "main.go"}},
	})
}

func TestMistralArray(t *testing.T) {
	content := `[TOOL_CALLS][{"name":"grep","arguments":{"path":"a.go","pattern":"foo"}}]`
	assertCalls(t, content, []Call{
		{Name: "grep", Args: map[string]interface{}{"path": "a.go", "pattern": "foo"}},
	})
}

func TestMistralNameArgs(t *testing.T) {
	content := `[TOOL_CALLS]read_lines[ARGS]{"path":"a.go","start":5}`
	assertCalls(t, content, []Call{
		{Name: "read_lines", Args: map[string]interface{}{"path": "a.go", "start": float64(5)}},
	})
}

func TestNemotronToolcall(t *testing.T) {
	content := `Let me look. <TOOLCALL>[{"name": "list_directory", "arguments": {"path": "."}}]</TOOLCALL>`
	assertCalls(t, content, []Call{
		{Name: "list_directory", Args: map[string]interface{}{"path": "."}},
	})
}

func TestHermesToolCallTag(t *testing.T) {
	content := "<tool_call>\n{\"name\": \"count_lines\", \"arguments\": {\"paths\": [\"a.go\"]}}\n</tool_call>"
	assertCalls(t, content, []Call{
		{Name: "count_lines", Args: map[string]interface{}{"paths": []interface{}{"a.go"}}},
	})
}

func TestGemmaToolCode(t *testing.T) {
	content := "I'll read it.\n```tool_code\nprint(read_file(path=\"main.go\"))\n```"
	assertCalls(t, content, []Call{
		{Name: "read_file", Args: map[string]interface{}{"path": "main.go"}},
	})
}

func TestGemmaToolCodeNumericAndBool(t *testing.T) {
	content := "```tool_code\ngrep(path=\"a.go\", pattern=\"x\", ignore_case=True)\n```"
	assertCalls(t, content, []Call{
		{Name: "grep", Args: map[string]interface{}{"path": "a.go", "pattern": "x", "ignore_case": true}},
	})
}

func TestLlamaBareJSON(t *testing.T) {
	content := `{"name": "grep", "parameters": {"path": "a.go", "pattern": "x"}}`
	assertCalls(t, content, []Call{
		{Name: "grep", Args: map[string]interface{}{"path": "a.go", "pattern": "x"}},
	})
}

func TestLlamaPythonTagJSON(t *testing.T) {
	content := pythonTag + `{"name": "read_file", "parameters": {"path": "main.go"}}` + "<|eom_id|>"
	assertCalls(t, content, []Call{
		{Name: "read_file", Args: map[string]interface{}{"path": "main.go"}},
	})
}

func TestLlamaParallelSemicolon(t *testing.T) {
	content := `{"name": "count_lines", "parameters": {"paths": ["a.go"]}}; ` +
		`{"name": "read_file", "parameters": {"path": "a.go"}}`
	assertCalls(t, content, []Call{
		{Name: "count_lines", Args: map[string]interface{}{"paths": []interface{}{"a.go"}}},
		{Name: "read_file", Args: map[string]interface{}{"path": "a.go"}},
	})
}

func TestLlamaPythonTagBuiltinCall(t *testing.T) {
	content := pythonTag + `grep.call(path="a.go", pattern="foo")`
	assertCalls(t, content, []Call{
		{Name: "grep", Args: map[string]interface{}{"path": "a.go", "pattern": "foo"}},
	})
}

func TestLlamaBareJSONNotAToolCall(t *testing.T) {
	// A JSON object lacking name+parameters must pass through untouched.
	content := `{"summary": "the assertion failed", "confidence": 0.9}`
	if got := Normalize(content); got != content {
		t.Errorf("Normalize rewrote non-tool-call JSON:\n got: %q\nwant: %q", got, content)
	}
}

func TestStructuredToolCalls(t *testing.T) {
	var raw []interface{}
	mustUnmarshal(t, `[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x.go\"}"}}]`, &raw)
	got := parseRendered(t, FromStructured(raw))
	want := []Call{{Name: "read_file", Args: map[string]interface{}{"path": "x.go"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FromStructured = %#v, want %#v", got, want)
	}
}

func TestNoToolCallPassthrough(t *testing.T) {
	content := "## Root Cause\nThe test asserts 2 but the function returns 3."
	if got := Normalize(content); got != content {
		t.Errorf("Normalize altered plain content:\n got: %q\nwant: %q", got, content)
	}
}

func TestMultipleHarmonyCalls(t *testing.T) {
	content := `to=functions.count_lines<|message|>{"paths":["a.go"]}<|call|>` +
		` then ` +
		`to=functions.read_file<|message|>{"path":"a.go"}<|call|>`
	assertCalls(t, content, []Call{
		{Name: "count_lines", Args: map[string]interface{}{"paths": []interface{}{"a.go"}}},
		{Name: "read_file", Args: map[string]interface{}{"path": "a.go"}},
	})
}

func mustUnmarshal(t *testing.T, s string, v interface{}) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("unmarshal %q: %v", s, err)
	}
}
