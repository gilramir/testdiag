// Package inspect drives a tool-using investigation loop ourselves instead of
// delegating to AgenticGoKit. Each turn sends exactly two messages — a static
// system prompt and a user message that is the freshly-rendered knowledge tree
// (see internal/knowledge) plus a next-step instruction. The knowledge tree
// replaces a growing message array: every fact a tool has returned is preserved
// and re-presented every turn, so the agent never loses what it has learned.
//
// This fixes the three failure modes of AGK v0.5.x's continuation loop, which
// (1) kept only the single most recent tool result, (2) discarded the original
// user message after the first tool call, and (3) actively told the model to
// stop calling tools.
package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/toolproto"
	"github.com/gilbertr/testdiag/internal/tools"
)

// Client sends a two-message (system + user) chat completion and returns the
// assistant's text content. Any structured tool_calls the server returns are
// folded into that text as canonical TOOL_CALL{...} markers so the caller parses
// tool calls uniformly via toolproto.Parse, regardless of whether the model
// emitted them natively, as structured calls, or as one of the open-model text
// syntaxes.
type Client interface {
	Chat(ctx context.Context, system, user string, schemas []tools.Schema) (string, error)
}

// httpClient talks to an OpenAI-compatible /chat/completions endpoint described
// by a config.LLMSpec. It speaks directly to the upstream model server — it is
// not routed through the llmproxy, because this package performs the proxy's two
// jobs (advertising tools and normalizing tool-call syntax) itself.
type httpClient struct {
	llm  config.LLMSpec
	http *http.Client
}

func newHTTPClient(llm config.LLMSpec) *httpClient {
	return &httpClient{llm: llm, http: &http.Client{Timeout: 10 * time.Minute}}
}

// Complete runs a single tool-less chat completion (system + user) against the
// given LLM and returns the assistant's text. It is the shared entry point for
// the tool-less stages (LOGPARSE, HYPOTHESIZE, SUMMARIZE, LESSONS, FEEDBACK, and
// MEMORIZE), which previously each built a throwaway AgenticGoKit agent with
// tools and memory disabled — exactly this call.
func Complete(ctx context.Context, llm config.LLMSpec, system, user string) (string, error) {
	return newHTTPClient(llm).Chat(ctx, system, user, nil)
}

func (c *httpClient) Chat(ctx context.Context, system, user string, schemas []tools.Schema) (string, error) {
	reqBody := map[string]interface{}{
		"model": c.llm.Model,
		"messages": []map[string]interface{}{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": c.llm.Temperature,
		"max_tokens":  c.llm.MaxTokens,
		"stream":      false,
	}
	if len(schemas) > 0 {
		reqBody["tools"] = toOpenAITools(schemas)
	}

	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	endpoint := strings.TrimRight(c.llm.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if c.llm.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.llm.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling LLM: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("LLM returned %d: %s", resp.StatusCode, truncate(string(body), 500))
	}

	return contentFromResponse(body)
}

// contentFromResponse extracts choices[0].message.content and appends any
// structured tool_calls (rewritten to TOOL_CALL text) so a downstream
// toolproto.Parse sees them.
func contentFromResponse(body []byte) (string, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string        `json:"content"`
				ToolCalls []interface{} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("response had no choices")
	}
	msg := parsed.Choices[0].Message
	content := msg.Content
	if len(msg.ToolCalls) > 0 {
		if rendered := toolproto.FromStructured(msg.ToolCalls); rendered != "" {
			content = strings.TrimSpace(content + "\n" + rendered)
		}
	}
	return content, nil
}

// toOpenAITools converts our tool schemas into the OpenAI `tools` array shape.
func toOpenAITools(schemas []tools.Schema) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        s.Name,
				"description": s.Description,
				"parameters":  s.Parameters,
			},
		})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
