// Package inspect drives a tool-using investigation loop ourselves. Each turn
// sends exactly two messages — a static system prompt and a user message that is
// the freshly-rendered knowledge tree (see internal/knowledge) plus a next-step
// instruction. The knowledge tree replaces a growing message array: every fact a
// tool has returned is preserved and re-presented every turn, so the agent never
// loses what it has learned.
//
// This deliberately avoids the three failure modes of a naive continuation loop,
// which would (1) keep only the single most recent tool result, (2) discard the
// original user message after the first tool call, and (3) pressure the model to
// stop calling tools.
package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gilramir/testdiag/internal/config"
	"github.com/gilramir/testdiag/internal/toolproto"
	"github.com/gilramir/testdiag/internal/tools"
)

// completeMu serializes debug output from concurrent Complete calls so each
// request/response block stays intact in the log.
var completeMu sync.Mutex

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
// MEMORIZE), each of which needs a single chat completion with no tools and no
// memory — exactly this call.
//
// Under -v it logs a one-line request and response heartbeat. Under --debug it
// logs the full system and user messages and the complete assistant reply.
func Complete(ctx context.Context, llm config.LLMSpec, system, user string) (string, error) {
	debug := tools.DebugEnabled()
	verbose := tools.VerboseEnabled()
	if debug {
		completeMu.Lock()
		fmt.Fprintf(os.Stderr, "\n========== LLM request (tool-less, %s) ==========\n", llm.Model)
		fmt.Fprintf(os.Stderr, "--- SYSTEM ---\n%s\n", system)
		fmt.Fprintf(os.Stderr, "--- USER ---\n%s\n", user)
		completeMu.Unlock()
	} else if verbose {
		fmt.Fprintf(os.Stderr, "[llm %s] -> tool-less: system(%dc) user(%dc)\n",
			llm.Model, len(system), len(user))
	}

	content, err := newHTTPClient(llm).Chat(ctx, system, user, nil)
	if err != nil {
		return "", err
	}

	if debug {
		completeMu.Lock()
		fmt.Fprintf(os.Stderr, "---------- LLM response ----------\n%s\n%s\n",
			strings.TrimSpace(content), strings.Repeat("=", 40))
		completeMu.Unlock()
	} else if verbose {
		fmt.Fprintf(os.Stderr, "[llm %s] <- text reply (%dc)\n", llm.Model, len(content))
	}
	return content, nil
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

	content, usage, err := contentFromResponse(body)
	if err != nil {
		return "", err
	}
	addUsage(usage)
	return content, nil
}

// contentFromResponse extracts choices[0].message.content, appends any
// structured tool_calls (rewritten to TOOL_CALL text) so a downstream
// toolproto.Parse sees them, and returns the token usage reported by the server.
func contentFromResponse(body []byte) (string, TokenUsage, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string        `json:"content"`
				ToolCalls []interface{} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", TokenUsage{}, fmt.Errorf("parsing response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", TokenUsage{}, fmt.Errorf("response had no choices")
	}
	msg := parsed.Choices[0].Message
	content := msg.Content
	if len(msg.ToolCalls) > 0 {
		if rendered := toolproto.FromStructured(msg.ToolCalls); rendered != "" {
			content = strings.TrimSpace(content + "\n" + rendered)
		}
	}
	usage := TokenUsage{
		Prompt:     parsed.Usage.PromptTokens,
		Completion: parsed.Usage.CompletionTokens,
		Total:      parsed.Usage.TotalTokens,
	}
	return content, usage, nil
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
