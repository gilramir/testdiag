package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

// chatHandler wraps httputil.ReverseProxy and intercepts chat-completions
// requests when the operator has queued an interrupt message. On interrupt it
// runs an interactive loop: the operator's message is injected into the
// conversation, the LLM replies, and the loop continues until the LLM calls a
// tool (normal flow resumes) or the operator accepts a text response as the
// final DEEPINSPECT output.
type chatHandler struct {
	rp        *httputil.ReverseProxy
	upstream  *url.URL
	tools     []map[string]interface{}
	normalize bool
	interrupt *InterruptController
}

func (h *chatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isChatCompletions(r.URL.Path) {
		// Re-inject accumulated operator notes into the system prompt on every
		// request so guidance from earlier interrupts survives across turns.
		if notes := h.interrupt.StickyNotes(); len(notes) > 0 {
			r = withStickyNotes(r, notes)
		}
		if trigger, ok := h.interrupt.TakeNonBlocking(); ok {
			msg := strings.TrimSpace(trigger)
			if msg == "" {
				// Empty Enter = pause signal: prompt for the actual message.
				fmt.Fprintf(os.Stdout, "\n\033[1;97;41m DEEPINSPECT PAUSED \033[0m\nEnter your message (or press Enter to resume): ")
				reply, ok := h.interrupt.ReadLine(r.Context())
				if !ok || strings.TrimSpace(reply) == "" {
					fmt.Fprintf(os.Stdout, "[resuming]\n\n")
					h.rp.ServeHTTP(w, r) // body not yet consumed; normal flow
					return
				}
				msg = strings.TrimSpace(reply)
			}
			h.runChatLoop(w, r, msg)
			return
		}
	}
	h.rp.ServeHTTP(w, r)
}

// withStickyNotes clones r and appends operator notes to its system message.
func withStickyNotes(r *http.Request, notes []string) *http.Request {
	body, _, ok := readJSONBody(r)
	if !ok {
		return r
	}
	msgs, _ := body["messages"].([]interface{})
	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if msg["role"] == "system" {
			var sb strings.Builder
			sb.WriteString(contentString(msg["content"]))
			sb.WriteString("\n\n## Operator notes\n")
			for _, note := range notes {
				sb.WriteString("- ")
				sb.WriteString(note)
				sb.WriteString("\n")
			}
			msg["content"] = sb.String()
			break
		}
	}
	body["messages"] = msgs
	out, err := json.Marshal(body)
	if err != nil {
		return r
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(out))
	r2.ContentLength = int64(len(out))
	r2.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
	return r2
}

func (h *chatHandler) runChatLoop(w http.ResponseWriter, r *http.Request, firstMsg string) {
	body, _, ok := readJSONBody(r)
	if !ok {
		// readJSONBody already restored r.Body; fall back to normal proxy.
		h.rp.ServeHTTP(w, r)
		return
	}

	// Apply the same per-request transforms that the Director applies.
	if len(h.tools) > 0 {
		injectTools(body, h.tools)
	}
	scrubContinuationNudge(body)

	fmt.Fprintf(os.Stdout, "\n\033[1;97;41m DEEPINSPECT INTERRUPTED \033[0m\n\n")

	// Persist the message so every subsequent request carries it.
	h.interrupt.AddStickyNote(firstMsg)

	msgs, _ := body["messages"].([]interface{})
	msgs = append(msgs, map[string]interface{}{
		"role":    "user",
		"content": "[Operator]: " + firstMsg,
	})
	body["messages"] = msgs

	for {
		respBody, err := h.callUpstream(r.Context(), r, body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nchat loop: upstream error: %v\n", err)
			http.Error(w, "upstream error during operator chat", http.StatusBadGateway)
			return
		}

		if h.normalize {
			normalizeChoices(respBody)
		}

		content, hasTools := extractContentAndTools(respBody)

		if hasTools {
			fmt.Fprintf(os.Stdout, "\033[1;33m[LLM called a tool — resuming DEEPINSPECT]\033[0m\n\n")
			writeJSONResponse(w, respBody)
			return
		}

		// Text response: show it and prompt for a reply or acceptance.
		fmt.Fprintf(os.Stdout, "\033[1;33m[LLM]:\033[0m\n%s\n\nReply (or press Enter to accept as final output): ",
			strings.TrimSpace(content))

		reply, ok := h.interrupt.ReadLine(r.Context())
		if !ok || strings.TrimSpace(reply) == "" {
			fmt.Fprintf(os.Stdout, "\033[1;33m[accepting as final DEEPINSPECT output]\033[0m\n\n")
			writeJSONResponse(w, respBody)
			return
		}

		reply = strings.TrimSpace(reply)
		h.interrupt.AddStickyNote(reply)
		msgs = body["messages"].([]interface{})
		msgs = append(msgs,
			map[string]interface{}{"role": "assistant", "content": content},
			map[string]interface{}{"role": "user", "content": "[Operator]: " + reply},
		)
		body["messages"] = msgs
	}
}

// callUpstream POSTs body directly to the upstream LLM, copying auth headers
// from the original AGK request. Returns the parsed JSON response body.
func (h *chatHandler) callUpstream(ctx context.Context, origReq *http.Request, body map[string]interface{}) (map[string]interface{}, error) {
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	upstreamURL := (&url.URL{
		Scheme: h.upstream.Scheme,
		Host:   h.upstream.Host,
		Path:   singleJoin(h.upstream.Path, origReq.URL.Path),
	}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(out))
	if err != nil {
		return nil, fmt.Errorf("building upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	for _, name := range []string{"Authorization", "X-Api-Key", "Api-Key"} {
		if v := origReq.Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling upstream: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading upstream response: %w", err)
	}
	var respBody map[string]interface{}
	if err := json.Unmarshal(raw, &respBody); err != nil {
		return nil, fmt.Errorf("parsing upstream response: %w", err)
	}
	return respBody, nil
}

// extractContentAndTools returns the text content of the first choice and
// whether the response contains any tool calls (structured tool_calls field or
// normalized TOOL_CALL text in content).
func extractContentAndTools(body map[string]interface{}) (content string, hasTools bool) {
	choices, ok := body["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return "", false
	}
	msg, ok := choice["message"].(map[string]interface{})
	if !ok {
		return "", false
	}
	content = contentString(msg["content"])
	if tc, ok := msg["tool_calls"].([]interface{}); ok && len(tc) > 0 {
		return content, true
	}
	if toolCallNameRE.MatchString(content) {
		return content, true
	}
	return content, false
}

func writeJSONResponse(w http.ResponseWriter, body map[string]interface{}) {
	out, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "JSON marshal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}
