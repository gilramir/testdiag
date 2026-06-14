// Package toolproto normalizes the many tool-calling "protocols" that
// open-weight models emit into the single canonical text form that
// AgenticGoKit's parser understands:
//
//	TOOL_CALL{"name":"read_file","args":{"path":"main.go"}}
//
// AgenticGoKit's OpenAI-compatible adapter does no native tool calling: it only
// returns assistant message *content*, and the agent's reasoning loop parses
// tool calls out of that text with a small set of recognized shapes (TOOL_CALL,
// `tool_name:`/`args:`, function-style, ReAct). Models such as GPT-OSS, Gemma,
// Mistral/devmistral and Nemotron each emit their own native syntax instead, so
// the loop never sees a tool call. Normalize rewrites those native syntaxes
// into the TOOL_CALL form, which the loop parses first and most reliably.
//
// Supported inputs:
//   - GPT-OSS Harmony:   ...to=functions.NAME...<|message|>{json}<|call|>
//   - Mistral:           [TOOL_CALLS][{"name":..,"arguments":..}]  or  [TOOL_CALLS]NAME[ARGS]{json}
//   - Nemotron / Hermes: <TOOLCALL>[{...}]</TOOLCALL>  or  <tool_call>{...}</tool_call>
//   - Gemma:             ```tool_code\nNAME(arg="v")\n```  (optionally print(...)-wrapped)
//   - Llama 3.x:         {"name":..,"parameters":..}  (bare, ; -separated for parallel),
//     optionally <|python_tag|>-prefixed, or <|python_tag|>NAME.call(arg="v")
//   - OpenAI structured: the response's choices[].message.tool_calls array (see FromStructured)
package toolproto

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Call is a normalized tool invocation.
type Call struct {
	Name string
	Args map[string]interface{}
}

// Render returns the canonical AgenticGoKit text form for one call.
func (c Call) Render() string {
	args := c.Args
	if args == nil {
		args = map[string]interface{}{}
	}
	b, _ := json.Marshal(struct {
		Name string                 `json:"name"`
		Args map[string]interface{} `json:"args"`
	}{c.Name, args})
	return "TOOL_CALL" + string(b)
}

// RenderAll joins rendered calls with newlines.
func RenderAll(calls []Call) string {
	parts := make([]string, len(calls))
	for i, c := range calls {
		parts[i] = c.Render()
	}
	return strings.Join(parts, "\n")
}

// detector rewrites any native tool-call spans in content into TOOL_CALL text,
// returning the new content and whether it changed anything.
type detector func(content string) (string, bool)

// detectors are tried in order; the first that matches wins. Ordered so the
// most syntactically distinctive markers are checked before the looser ones.
var detectors = []detector{
	detectHarmony,
	detectMistral,
	detectTaggedJSON,
	detectGemma,
	detectLlama, // last: its bare-JSON form has no distinctive marker
}

// Normalize rewrites the first recognized native tool-call protocol found in
// content into canonical TOOL_CALL text. If nothing matches (e.g. the model's
// final natural-language answer), content is returned unchanged.
func Normalize(content string) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	for _, d := range detectors {
		if out, ok := d(content); ok {
			return out
		}
	}
	return content
}

// Parse normalizes content (rewriting any native tool-call syntax into the
// canonical TOOL_CALL form) and then extracts every TOOL_CALL{...} occurrence
// into a Call. It returns nil when the content carries no tool call — i.e. the
// model produced a final natural-language answer. This is the entry point for a
// caller that drives the tool loop itself rather than handing content to
// AgenticGoKit's parser.
func Parse(content string) []Call {
	normalized := Normalize(content)
	var calls []Call
	rest := normalized
	for {
		idx := strings.Index(rest, "TOOL_CALL")
		if idx < 0 {
			break
		}
		after := rest[idx+len("TOOL_CALL"):]
		obj, end := firstJSONObject(after)
		if obj == nil {
			// No JSON object follows this marker; skip past it and keep looking.
			rest = after
			continue
		}
		if c, ok := callFromMap(obj); ok {
			calls = append(calls, c)
		}
		rest = after[end:]
	}
	return calls
}

// FromStructured converts an OpenAI-style choices[].message.tool_calls array
// (each element shaped like {"function":{"name":..,"arguments":"{...}"}}) into
// canonical TOOL_CALL text. Used by the proxy when a server returns structured
// tool calls, which the OpenAI adapter would otherwise drop.
func FromStructured(raw []interface{}) string {
	var calls []Call
	for _, item := range raw {
		if m, ok := item.(map[string]interface{}); ok {
			if c, ok := callFromMap(m); ok {
				calls = append(calls, c)
			}
		}
	}
	return RenderAll(calls)
}

// ---------------------------------------------------------------------------
// GPT-OSS Harmony
// ---------------------------------------------------------------------------

var harmonyRe = regexp.MustCompile(`(?s)to=functions\.([A-Za-z0-9_.\-]+).*?<\|message\|>(.*?)<\|(?:call|end|return)\|>`)

func detectHarmony(content string) (string, bool) {
	locs := harmonyRe.FindAllStringSubmatchIndex(content, -1)
	if len(locs) == 0 {
		return content, false
	}
	var b strings.Builder
	last := 0
	for _, m := range locs {
		name := content[m[2]:m[3]]
		args := parseJSONObject(content[m[4]:m[5]])
		b.WriteString(content[last:m[0]])
		b.WriteString(Call{Name: name, Args: args}.Render())
		last = m[1]
	}
	b.WriteString(content[last:])
	return b.String(), true
}

// ---------------------------------------------------------------------------
// Mistral / devmistral
// ---------------------------------------------------------------------------

func detectMistral(content string) (string, bool) {
	idx := strings.Index(content, "[TOOL_CALLS]")
	if idx < 0 {
		return content, false
	}
	rest := strings.TrimLeft(content[idx+len("[TOOL_CALLS]"):], " \t\r\n")

	var calls []Call
	if strings.HasPrefix(rest, "[") {
		if arr, _ := firstJSONArray(rest); arr != nil {
			calls = callsFromArray(arr)
		}
	} else {
		calls = parseMistralNameArgs(rest)
	}
	if len(calls) == 0 {
		return content, false
	}
	return content[:idx] + RenderAll(calls), true
}

// parseMistralNameArgs handles the raw-token form: NAME[ARGS]{json}, possibly
// repeated and possibly re-prefixed with another [TOOL_CALLS].
func parseMistralNameArgs(s string) []Call {
	var calls []Call
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		s = strings.TrimPrefix(s, "[TOOL_CALLS]")
		s = strings.TrimLeft(s, " \t\r\n")
		ai := strings.Index(s, "[ARGS]")
		if ai < 0 {
			break
		}
		name := strings.TrimSpace(s[:ai])
		after := s[ai+len("[ARGS]"):]
		obj, end := firstJSONObject(after)
		if name == "" || obj == nil {
			break
		}
		calls = append(calls, Call{Name: name, Args: obj})
		s = after[end:]
	}
	return calls
}

// ---------------------------------------------------------------------------
// Nemotron <TOOLCALL>…</TOOLCALL> and Hermes/Qwen <tool_call>…</tool_call>
// ---------------------------------------------------------------------------

var toolTagRe = regexp.MustCompile(`(?is)<\s*tool_?call\s*>(.*?)<\s*/\s*tool_?call\s*>`)

func detectTaggedJSON(content string) (string, bool) {
	locs := toolTagRe.FindAllStringSubmatchIndex(content, -1)
	if len(locs) == 0 {
		return content, false
	}
	var b strings.Builder
	last := 0
	matched := false
	for _, m := range locs {
		calls := callsFromJSONBlob(content[m[2]:m[3]])
		if len(calls) == 0 {
			continue
		}
		b.WriteString(content[last:m[0]])
		b.WriteString(RenderAll(calls))
		last = m[1]
		matched = true
	}
	if !matched {
		return content, false
	}
	b.WriteString(content[last:])
	return b.String(), true
}

// ---------------------------------------------------------------------------
// Gemma ```tool_code … ```
// ---------------------------------------------------------------------------

var (
	toolCodeRe = regexp.MustCompile("(?s)```tool_code\\s*(.*?)```")
	pyCallRe   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*\((.*)\)\s*$`)
)

func detectGemma(content string) (string, bool) {
	locs := toolCodeRe.FindAllStringSubmatchIndex(content, -1)
	if len(locs) == 0 {
		return content, false
	}
	var b strings.Builder
	last := 0
	matched := false
	for _, m := range locs {
		calls := parsePyCalls(content[m[2]:m[3]])
		if len(calls) == 0 {
			continue
		}
		b.WriteString(content[last:m[0]])
		b.WriteString(RenderAll(calls))
		last = m[1]
		matched = true
	}
	if !matched {
		return content, false
	}
	b.WriteString(content[last:])
	return b.String(), true
}

func parsePyCalls(s string) []Call {
	var calls []Call
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Gemma often wraps the call in print(...): unwrap one layer.
		if strings.HasPrefix(line, "print(") && strings.HasSuffix(line, ")") {
			line = line[len("print(") : len(line)-1]
		}
		m := pyCallRe.FindStringSubmatch(line)
		if m == nil || m[1] == "print" {
			continue
		}
		calls = append(calls, Call{Name: m[1], Args: parsePyArgs(m[2])})
	}
	return calls
}

func parsePyArgs(s string) map[string]interface{} {
	args := map[string]interface{}{}
	for _, part := range splitTopLevel(s) {
		part = strings.TrimSpace(part)
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(part[:eq])
		if key == "" {
			continue
		}
		args[key] = pyValue(strings.TrimSpace(part[eq+1:]))
	}
	return args
}

func pyValue(v string) interface{} {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	switch v {
	case "True", "true":
		return true
	case "False", "false":
		return false
	case "None", "null", "":
		return nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return v
}

// splitTopLevel splits on commas that are not inside quotes or brackets.
func splitTopLevel(s string) []string {
	var parts []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// ---------------------------------------------------------------------------
// Llama 3.x
//
// Custom tools are emitted as a bare JSON object {"name":..,"parameters":..}
// (parallel calls separated by ";"). The ipython/built-in path prefixes the
// call with the <|python_tag|> special token and may use a Python invocation
// like brave_search.call(query="..").
// ---------------------------------------------------------------------------

const pythonTag = "<|python_tag|>"

var llamaPyCallRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.]*)\s*\((.*)\)\s*$`)

func detectLlama(content string) (string, bool) {
	if idx := strings.Index(content, pythonTag); idx >= 0 {
		calls := parseLlamaBody(trimLlamaEnd(content[idx+len(pythonTag):]))
		if len(calls) == 0 {
			return content, false
		}
		return content[:idx] + RenderAll(calls), true
	}

	// Bare JSON: the whole assistant message is the call. Require a leading '{'
	// and a name + parameters on each object so we don't rewrite incidental JSON
	// in a final answer.
	if !strings.HasPrefix(strings.TrimSpace(content), "{") {
		return content, false
	}
	calls := parseLlamaJSONList(content)
	if len(calls) == 0 {
		return content, false
	}
	return RenderAll(calls), true
}

func parseLlamaBody(s string) []Call {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		return parseLlamaJSONList(s)
	}
	if m := llamaPyCallRe.FindStringSubmatch(s); m != nil {
		name := strings.TrimSuffix(m[1], ".call")
		return []Call{{Name: name, Args: parsePyArgs(m[2])}}
	}
	return nil
}

func parseLlamaJSONList(s string) []Call {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		arr, _ := firstJSONArray(s)
		var calls []Call
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if c, ok := llamaCallFromMap(m); ok {
					calls = append(calls, c)
				}
			}
		}
		return calls
	}

	var calls []Call
	for {
		s = strings.TrimLeft(s, " \t\r\n;")
		if !strings.HasPrefix(s, "{") {
			break
		}
		obj, end := firstJSONObject(s)
		if obj == nil {
			break
		}
		c, ok := llamaCallFromMap(obj)
		if !ok {
			break
		}
		calls = append(calls, c)
		s = s[end:]
	}
	return calls
}

// llamaCallFromMap is the strict variant used for Llama's marker-less JSON: a
// call must carry a string name and an explicit parameters/arguments object.
func llamaCallFromMap(m map[string]interface{}) (Call, bool) {
	name, _ := m["name"].(string)
	if name == "" {
		return Call{}, false
	}
	for _, k := range []string{"parameters", "arguments", "args"} {
		if args, ok := m[k].(map[string]interface{}); ok {
			return Call{Name: name, Args: args}, true
		}
	}
	return Call{}, false
}

func trimLlamaEnd(s string) string {
	for _, m := range []string{"<|eom_id|>", "<|eot_id|>", "<|end|>"} {
		if i := strings.Index(s, m); i >= 0 {
			s = s[:i]
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// shared JSON helpers
// ---------------------------------------------------------------------------

// callFromMap extracts a Call from a decoded JSON object, tolerating the
// common field-name variations (name/function.name, arguments/args/parameters)
// and arguments delivered as a JSON-encoded string.
func callFromMap(m map[string]interface{}) (Call, bool) {
	name, _ := m["name"].(string)
	if name == "" {
		if fn, ok := m["function"].(map[string]interface{}); ok {
			name, _ = fn["name"].(string)
			m = fn
		}
	}
	if name == "" {
		return Call{}, false
	}

	var args map[string]interface{}
	for _, k := range []string{"arguments", "args", "parameters"} {
		switch v := m[k].(type) {
		case map[string]interface{}:
			args = v
		case string:
			args = parseJSONObject(v)
		}
		if args != nil {
			break
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	return Call{Name: name, Args: args}, true
}

func callsFromArray(arr []interface{}) []Call {
	var calls []Call
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok {
			if c, ok := callFromMap(m); ok {
				calls = append(calls, c)
			}
		}
	}
	return calls
}

// callsFromJSONBlob parses a JSON object or array of objects into calls.
func callsFromJSONBlob(s string) []Call {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		if arr, _ := firstJSONArray(s); arr != nil {
			return callsFromArray(arr)
		}
		return nil
	}
	if obj, _ := firstJSONObject(s); obj != nil {
		if c, ok := callFromMap(obj); ok {
			return []Call{c}
		}
	}
	return nil
}

func parseJSONObject(s string) map[string]interface{} {
	obj, _ := firstJSONObject(s)
	return obj
}

// firstJSONObject finds the first balanced {...} in s (respecting string
// literals), unmarshals it, and returns it with the index just past its close.
func firstJSONObject(s string) (map[string]interface{}, int) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil, 0
	}
	end := matchBracket(s, start, '{', '}')
	if end < 0 {
		return nil, 0
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(s[start:end+1]), &obj); err != nil {
		return nil, 0
	}
	return obj, end + 1
}

// firstJSONArray finds the first balanced [...] in s, unmarshals it, and
// returns it with the index just past its close.
func firstJSONArray(s string) ([]interface{}, int) {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return nil, 0
	}
	end := matchBracket(s, start, '[', ']')
	if end < 0 {
		return nil, 0
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(s[start:end+1]), &arr); err != nil {
		return nil, 0
	}
	return arr, end + 1
}

// matchBracket returns the index of the bracket that closes the open bracket at
// position start, honoring string literals and escapes, or -1 if unbalanced.
func matchBracket(s string, start int, open, close byte) int {
	depth := 0
	var quote byte
	for i := start; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++ // skip escaped char
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
