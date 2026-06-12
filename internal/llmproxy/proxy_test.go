package llmproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProxyRoundTrip verifies the proxy re-prefixes the upstream path, injects
// the tools array into the request, and rewrites a native (Mistral) tool call
// in the response into canonical TOOL_CALL text that the OpenAI adapter (which
// reads only message.content) will see.
func TestProxyRoundTrip(t *testing.T) {
	var gotPath string
	var gotTools []interface{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]interface{}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotTools, _ = body["tools"].([]interface{})

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant",`+
			`"content":"[TOOL_CALLS]read_file[ARGS]{\"path\":\"main.go\"}"},`+
			`"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	px, err := Start(upstream.URL+"/v1", Options{
		Tools: []Tool{
			{Name: "read_file", Description: "read a file", Parameters: map[string]interface{}{"type": "object"}},
		},
		Normalize: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer px.Close()

	// Mimic what AgenticGoKit's OpenAI adapter does: POST BaseURL+/chat/completions.
	resp, err := http.Post(px.BaseURL()+"/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if len(gotTools) != 1 {
		t.Errorf("upstream received %d tools, want 1", len(gotTools))
	}

	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, out)
	}
	content := decoded.Choices[0].Message.Content
	if !strings.Contains(content, `TOOL_CALL{"name":"read_file"`) {
		t.Errorf("response content not normalized: %q", content)
	}
}

// TestScrubContinuationNudge verifies the proxy rewrites AgenticGoKit's
// "Do NOT make additional tool calls" nudge out of an outgoing request so it
// never reaches the model.
func TestScrubContinuationNudge(t *testing.T) {
	var gotContent string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		msgs, _ := body["messages"].([]interface{})
		if len(msgs) > 0 {
			if m, ok := msgs[0].(map[string]interface{}); ok {
				gotContent, _ = m["content"].(string)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	px, err := Start(upstream.URL+"/v1", Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer px.Close()

	reqBody := `{"model":"m","messages":[{"role":"user","content":` +
		`"Tool execution results:\nfoo\n\nBased on these tool results, ` +
		`provide a final answer. Do NOT make additional tool calls unless absolutely necessary."}]}`
	resp, err := http.Post(px.BaseURL()+"/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	if strings.Contains(gotContent, "Do NOT make additional tool calls") {
		t.Errorf("nudge reached upstream: %q", gotContent)
	}
	if !strings.Contains(gotContent, "keep investigating") {
		t.Errorf("replacement not applied: %q", gotContent)
	}
}
