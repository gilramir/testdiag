// Package llmproxy runs a tiny in-process reverse proxy in front of an
// OpenAI-compatible LLM endpoint so that testdiag works with models whose
// native tool-calling syntax the tool loop would not otherwise recognize.
//
// Why this exists: a tool loop that reads only choices[].message.content and
// parses tool calls out of that text never sees the native tool-call syntaxes
// emitted by models like GPT-OSS, Gemma, Mistral and Nemotron. This proxy sits
// between the client and the real server and fixes both ends:
//
//   - Request side:  injects a `tools` array (so tool-aware chat templates
//     advertise the tools to the model) when tool schemas are provided.
//   - Response side:  rewrites whatever tool-call format the model emitted —
//     native syntax in the content, or a structured tool_calls field — into the
//     canonical TOOL_CALL{...} text the tool loop reliably parses (see toolproto).
//
// Because every request and response flows through here, it is also the natural
// place to log the full conversation with the LLM for debugging (Options.Debug).
//
// Point cfg.LLM.BaseURL at BaseURL() and the rest of the program is unchanged.
package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gilramir/testdiag/internal/toolproto"
)

// Tool is a tool definition advertised to the model via the request's `tools`
// array. Parameters is a JSON Schema object.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
}

// Options configures a proxy.
type Options struct {
	// Tools, if non-empty, is injected into every chat-completions request.
	Tools []Tool
	// Normalize rewrites each model's native tool-call syntax in responses into
	// the canonical TOOL_CALL text the tool loop parses. When false the proxy
	// passes responses through unchanged (useful when only Debug is wanted).
	Normalize bool
	// Debug logs the full request/response conversation with the LLM to stderr.
	Debug bool
	// Verbose logs a one-line heartbeat per LLM request and response (message and
	// tool counts, and any tool calls requested) so an operator can see the
	// round-trips and tell a slow LLM call from a running tool. Debug supersedes
	// it (the full log already shows everything).
	Verbose bool
	// Interrupt, if non-nil, enables operator chat mode: when the operator types
	// a line into stdin, the next outgoing LLM request is intercepted and the
	// line is injected as a user message. The loop continues until the LLM calls
	// a tool (normal flow resumes) or the operator accepts a text reply as the
	// final output. Only set this on the DEEPINSPECT proxy.
	Interrupt *InterruptController
}

// Proxy is a running normalizing reverse proxy. Close it when done.
type Proxy struct {
	listener   net.Listener
	server     *http.Server
	baseURL    string
	reqCounter atomic.Uint64
}

// ResetCounter resets the per-proxy request counter to zero. Call this at the
// start of each agent run so the [llm] heartbeat lines show per-run sequence
// numbers rather than a monotone counter spanning all runs on this proxy.
func (p *Proxy) ResetCounter() { p.reqCounter.Store(0) }

// debugIDKey tags a request's context with its debug sequence number so the
// response logged later can be correlated with the request that produced it.
type debugIDKey struct{}

// proxyLabelKey tags a request's context with the emitting proxy's label so the
// response heartbeat (logged from ModifyResponse, which has no Proxy handle) can
// name the same proxy the request did.
type proxyLabelKey struct{}

// label is the short proxy identifier shown in heartbeat lines (its listen
// port). Because proxies are keyed by (endpoint, tool set), the port is enough
// to tell, e.g., the tool-using PLANINSPECTION proxy from the tool-less FEEDBACK
// proxy whose requests interleave in the same console phase — both first
// requests are otherwise "2 message(s)". Correlate the port with the startup
// "LLM proxy active: …:PORT … tools=N" banner to see which tool set it serves.
func (p *Proxy) label() string {
	if p.listener == nil {
		return "?"
	}
	if addr, ok := p.listener.Addr().(*net.TCPAddr); ok {
		return fmt.Sprintf(":%d", addr.Port)
	}
	return p.listener.Addr().String()
}

// Start launches the proxy in front of upstreamBaseURL (e.g.
// http://localhost:1234/v1), listening on an ephemeral localhost port. The
// proxy is serving by the time Start returns.
func Start(upstreamBaseURL string, opts Options) (*Proxy, error) {
	target, err := url.Parse(strings.TrimSuffix(upstreamBaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parsing upstream base URL %q: %w", upstreamBaseURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("upstream base URL %q must be absolute (scheme://host)", upstreamBaseURL)
	}

	openAITools := toOpenAITools(opts.Tools)
	debug := opts.Debug
	verbose := opts.Verbose
	normalize := opts.Normalize

	// p is created before the Director closure so the closure can reference
	// p.reqCounter, which main can reset between agent runs.
	p := &Proxy{}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			reqPath := req.URL.Path
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = singleJoin(target.Path, reqPath)
			req.URL.RawPath = ""
			req.Host = target.Host
			// Ask for an unencoded body so ModifyResponse can rewrite it.
			req.Header.Set("Accept-Encoding", "identity")

			isChat := isChatCompletions(reqPath)
			inject := isChat && len(openAITools) > 0
			heartbeat := verbose && isChat && !debug
			if !isChat && !debug {
				return
			}
			body, raw, ok := readJSONBody(req)
			if !ok {
				return // readJSONBody restored the body as-is
			}
			if inject {
				injectTools(body, openAITools)
			}
			if isChat {
				scrubContinuationNudge(body)
			}
			if debug || heartbeat {
				id := p.reqCounter.Add(1)
				ctx := context.WithValue(req.Context(), debugIDKey{}, id)
				ctx = context.WithValue(ctx, proxyLabelKey{}, p.label())
				*req = *req.WithContext(ctx)
				if debug {
					logRequest(id, reqPath, body)
				} else {
					logRequestBrief(id, p.label(), reqPath, body)
				}
			}
			setBody(req, body, raw)
		},
		ModifyResponse: func(resp *http.Response) error {
			return modifyResponse(resp, normalize, debug, verbose)
		},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("starting proxy listener: %w", err)
	}
	var handler http.Handler = rp
	if opts.Interrupt != nil {
		handler = &chatHandler{
			rp:        rp,
			upstream:  target,
			tools:     openAITools,
			normalize: normalize,
			interrupt: opts.Interrupt,
		}
	}
	p.listener = ln
	p.server = &http.Server{Handler: handler}
	p.baseURL = fmt.Sprintf("http://%s", ln.Addr().String())
	go p.server.Serve(ln)
	return p, nil
}

// BaseURL is the local URL to use as the LLM base URL. It carries no path
// suffix; the adapter appends "/chat/completions" and the proxy re-prefixes the
// upstream path.
func (p *Proxy) BaseURL() string { return p.baseURL }

// Close shuts the proxy down.
func (p *Proxy) Close() error { return p.server.Close() }

// injectTools adds (or augments) the request's `tools` array and defaults
// tool_choice to "auto", leaving any caller-supplied values intact.
func injectTools(body map[string]interface{}, tools []map[string]interface{}) {
	if _, exists := body["tools"]; !exists {
		body["tools"] = tools
	}
	if _, exists := body["tool_choice"]; !exists {
		body["tool_choice"] = "auto"
	}
}

// continuationNudge is a "stop calling tools" instruction a continuation prompt
// can append to the user message after a tool call. It pressures the model to
// stop calling tools, which cuts a multi-step diagnosis short before the agent
// has read enough source, so the proxy rewrites it on the way out — see
// scrubContinuationNudge.
const continuationNudge = "provide a final answer. Do NOT make additional tool calls unless absolutely necessary."

// continuationReplacement is the permissive instruction we substitute, telling
// the agent to keep investigating until it has the root cause.
const continuationReplacement = "keep investigating: call more tools whenever you need more evidence, and give your final answer only once you have found the root cause."

// scrubContinuationNudge replaces a "stop calling tools" nudge in every
// message's content with a permissive instruction. Returns whether it changed
// anything. Content may be a plain string or an array of typed parts.
func scrubContinuationNudge(body map[string]interface{}) bool {
	msgs, ok := body["messages"].([]interface{})
	if !ok {
		return false
	}
	changed := false
	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			if out, ok := stripNudge(c); ok {
				msg["content"] = out
				changed = true
			}
		case []interface{}:
			for _, part := range c {
				pm, ok := part.(map[string]interface{})
				if !ok {
					continue
				}
				if text, ok := pm["text"].(string); ok {
					if out, ok := stripNudge(text); ok {
						pm["text"] = out
						changed = true
					}
				}
			}
		}
	}
	return changed
}

func stripNudge(s string) (string, bool) {
	if !strings.Contains(s, continuationNudge) {
		return s, false
	}
	return strings.ReplaceAll(s, continuationNudge, continuationReplacement), true
}

// modifyResponse optionally normalizes tool-call syntax in a chat-completions
// JSON response and/or logs it. Non-JSON and streaming responses pass through
// untouched.
func modifyResponse(resp *http.Response, normalize, debug, verbose bool) error {
	if !normalize && !debug && !verbose {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		return nil // streaming (text/event-stream) or anything non-JSON
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		// Not the shape we expected; pass it through verbatim.
		restoreBody(resp, raw)
		return nil
	}

	changed := false
	if normalize {
		changed = normalizeChoices(body)
	}
	if debug {
		id, _ := resp.Request.Context().Value(debugIDKey{}).(uint64)
		logResponse(id, body)
	} else if verbose {
		id, _ := resp.Request.Context().Value(debugIDKey{}).(uint64)
		label, _ := resp.Request.Context().Value(proxyLabelKey{}).(string)
		logResponseBrief(id, label, body)
	}

	if !changed {
		restoreBody(resp, raw)
		return nil
	}
	out, err := json.Marshal(body)
	if err != nil {
		restoreBody(resp, raw)
		return nil
	}
	restoreBody(resp, out)
	return nil
}

// normalizeChoices folds tool calls in every choice's message into canonical
// TOOL_CALL text. Returns whether anything changed.
func normalizeChoices(body map[string]interface{}) bool {
	changed := false
	if choices, ok := body["choices"].([]interface{}); ok {
		for _, ch := range choices {
			choice, ok := ch.(map[string]interface{})
			if !ok {
				continue
			}
			msg, ok := choice["message"].(map[string]interface{})
			if !ok {
				continue
			}
			if rewriteMessage(msg) {
				changed = true
			}
		}
	}
	return changed
}

// rewriteMessage folds a structured tool_calls field and any native tool-call
// syntax in the content into TOOL_CALL text. Returns whether it changed msg.
func rewriteMessage(msg map[string]interface{}) bool {
	content, _ := msg["content"].(string)
	var pieces []string

	if tc, ok := msg["tool_calls"].([]interface{}); ok && len(tc) > 0 {
		if text := toolproto.FromStructured(tc); text != "" {
			pieces = append(pieces, text)
			delete(msg, "tool_calls")
		}
	}

	normalized := toolproto.Normalize(content)
	if normalized != content || len(pieces) > 0 {
		pieces = append(pieces, normalized)
		msg["content"] = strings.TrimLeft(strings.Join(pieces, "\n"), "\n")
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// debug logging
// ---------------------------------------------------------------------------

// debugMu serializes debug output so each request/response block stays intact
// even when multiple workers drive the proxy concurrently.
var debugMu sync.Mutex

// logRequest prints the messages (and any tool calls) of an outgoing request.
func logRequest(id uint64, path string, body map[string]interface{}) {
	debugMu.Lock()
	defer debugMu.Unlock()

	fmt.Fprintf(os.Stderr, "\n========== LLM request #%d  %s ==========\n", id, path)
	msgs, _ := body["messages"].([]interface{})
	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		fmt.Fprintf(os.Stderr, "--- %s ---\n", strings.ToUpper(role))
		if c := contentString(msg["content"]); c != "" {
			fmt.Fprintln(os.Stderr, c)
		}
		if tc, ok := msg["tool_calls"].([]interface{}); ok {
			for _, t := range tc {
				logToolCall(t)
			}
		}
	}
}

// logResponse prints the assistant message(s) of a response.
func logResponse(id uint64, body map[string]interface{}) {
	debugMu.Lock()
	defer debugMu.Unlock()

	fmt.Fprintf(os.Stderr, "\n---------- LLM response #%d ----------\n", id)
	choices, _ := body["choices"].([]interface{})
	for _, ch := range choices {
		choice, ok := ch.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]interface{})
		if !ok {
			continue
		}
		if c := contentString(msg["content"]); c != "" {
			fmt.Fprintln(os.Stderr, c)
		}
		if tc, ok := msg["tool_calls"].([]interface{}); ok {
			for _, t := range tc {
				logToolCall(t)
			}
		}
	}
	fmt.Fprintln(os.Stderr, strings.Repeat("=", 40))
}

// toolCallNameRE extracts tool names from canonical TOOL_CALL text so the brief
// response heartbeat can report which tools the model asked for.
var toolCallNameRE = regexp.MustCompile(`TOOL_CALL\s*\{\s*"name"\s*:\s*"([^"]+)"`)

// logRequestBrief prints a one-line heartbeat for an outgoing request: message
// count and advertised tool count. Used in verbose (non-debug) mode.
func logRequestBrief(id uint64, label, path string, body map[string]interface{}) {
	debugMu.Lock()
	defer debugMu.Unlock()
	msgs, _ := body["messages"].([]interface{})
	tools, _ := body["tools"].([]interface{})
	fmt.Fprintf(os.Stderr, "[llm %s] -> request #%d %s: %d message(s), %d tool(s) advertised\n",
		label, id, path, len(msgs), len(tools))
}

// logResponseBrief prints a one-line heartbeat for a response: which tool(s) the
// model asked for, or the size of a plain-text answer, plus finish_reason.
func logResponseBrief(id uint64, label string, body map[string]interface{}) {
	debugMu.Lock()
	defer debugMu.Unlock()
	desc := "no choices"
	if choices, ok := body["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			desc = describeChoiceBrief(choice)
		}
	}
	fmt.Fprintf(os.Stderr, "[llm %s] <- response #%d: %s\n", label, id, desc)
}

// describeChoiceBrief summarizes one response choice: the tool calls it requests
// (from normalized TOOL_CALL text, or a still-structured tool_calls field), or
// the length of a plain-text reply, with the finish_reason appended.
func describeChoiceBrief(choice map[string]interface{}) string {
	finish, _ := choice["finish_reason"].(string)
	suffix := ""
	if finish != "" {
		suffix = ", finish=" + finish
	}
	msg, ok := choice["message"].(map[string]interface{})
	if !ok {
		return "no message" + suffix
	}

	var names []string
	content := contentString(msg["content"])
	for _, m := range toolCallNameRE.FindAllStringSubmatch(content, -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		if tc, ok := msg["tool_calls"].([]interface{}); ok {
			for _, t := range tc {
				if tm, ok := t.(map[string]interface{}); ok {
					if fn, ok := tm["function"].(map[string]interface{}); ok {
						if n, ok := fn["name"].(string); ok {
							names = append(names, n)
						}
					}
				}
			}
		}
	}
	if len(names) > 0 {
		return "tool call(s): " + strings.Join(names, ", ") + suffix
	}
	return fmt.Sprintf("text reply (%d chars)%s", len(content), suffix)
}

func logToolCall(t interface{}) {
	tc, ok := t.(map[string]interface{})
	if !ok {
		return
	}
	fn, ok := tc["function"].(map[string]interface{})
	if !ok {
		return
	}
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	fmt.Fprintf(os.Stderr, "→ tool_call %s(%s)\n", name, args)
}

// contentString renders an OpenAI message "content", which may be a plain
// string or an array of typed parts, into displayable text.
func contentString(v interface{}) string {
	switch c := v.(type) {
	case string:
		return c
	case []interface{}:
		var b strings.Builder
		for _, part := range c {
			if pm, ok := part.(map[string]interface{}); ok {
				if text, ok := pm["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		out, _ := json.Marshal(v)
		return string(out)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func toOpenAITools(tools []Tool) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		params := t.Parameters
		if params == nil {
			params = map[string]interface{}{"type": "object"}
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

func isChatCompletions(path string) bool {
	return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/completions")
}

// singleJoin joins two URL path segments with exactly one slash.
func singleJoin(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
	}
}

// readJSONBody reads and decodes a request's JSON body. On any read/decode
// problem it restores the body as-is and reports ok=false so the caller leaves
// the request untouched.
func readJSONBody(req *http.Request) (body map[string]interface{}, raw []byte, ok bool) {
	if req.Body == nil {
		return nil, nil, false
	}
	raw, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		req.Body = io.NopCloser(bytes.NewReader(raw))
		return nil, raw, false
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.ContentLength = int64(len(raw))
		return nil, raw, false
	}
	return body, raw, true
}

func setBody(req *http.Request, body map[string]interface{}, fallback []byte) {
	out, err := json.Marshal(body)
	if err != nil {
		out = fallback
	}
	req.Body = io.NopCloser(bytes.NewReader(out))
	req.ContentLength = int64(len(out))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
}

func restoreBody(resp *http.Response, data []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))
	// We requested identity upstream; make sure no stale encoding header remains.
	resp.Header.Del("Content-Encoding")
}
