package inspect

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/knowledge"
	"github.com/gilbertr/testdiag/internal/toolproto"
	"github.com/gilbertr/testdiag/internal/tools"
)

// Interrupter lets an operator inject guidance into a running investigation. It
// is satisfied by *llmproxy.InterruptController; declared here as an interface
// so this package does not import llmproxy. Sticky notes are re-injected into
// the system prompt every turn so guidance persists for the rest of the run.
type Interrupter interface {
	TakeNonBlocking() (string, bool)
	ReadLine(ctx context.Context) (string, bool)
	AddStickyNote(string)
	StickyNotes() []string
}

// Options configures an Engine. Verbose/debug logging is not configured here;
// the engine consults the process-global flags set via tools.SetVerbose /
// tools.SetDebug (the same flags the workspace tools use), so its fact-tree and
// response logging always tracks the operator's -v/--debug choice.
type Options struct {
	MaxIterations int            // cap on tool rounds
	MaxChars      int            // knowledge render budget (0 = unlimited)
	Schemas       []tools.Schema // tools advertised to the model
	Interrupt     Interrupter    // optional operator-interrupt console
}

// Engine runs a tool-using investigation as our own loop, accumulating facts in
// a knowledge.Store that is re-rendered into the context every turn.
type Engine struct {
	client    Client
	maxIter   int
	maxChars  int
	schemas   []tools.Schema
	interrupt Interrupter
}

// NewEngine builds an Engine that talks to the given LLM directly (not through
// the llmproxy). Schemas are the tools advertised to the model, e.g.
// tools.SchemasExcluding(append(LogToolNames, "notebook")...).
func NewEngine(llm config.LLMSpec, opts Options) *Engine {
	return newEngineWithClient(newHTTPClient(llm), opts)
}

// newEngineWithClient is used by tests to inject a scripted Client.
func newEngineWithClient(c Client, opts Options) *Engine {
	return &Engine{
		client:    c,
		maxIter:   opts.MaxIterations,
		maxChars:  opts.MaxChars,
		schemas:   opts.Schemas,
		interrupt: opts.Interrupt,
	}
}

// RunInput is one investigation.
type RunInput struct {
	System string // static system prompt (brief + hypothesis + plan + source …)
	Task   string // the objective, restated to the model every turn
}

// Result is the outcome of a run.
type Result struct {
	Content     string           // the model's final analysis text
	ToolsCalled []string         // distinct tools invoked, in first-call order
	Store       *knowledge.Store // accumulated facts (for a debug dump)
	Iterations  int              // tool rounds actually performed
}

// Run drives the loop: render knowledge → ask the model → if it calls tools,
// execute and fold them in; if it answers, return. After maxIter tool rounds it
// asks once more with no tools available, forcing a final answer.
func (e *Engine) Run(ctx context.Context, in RunInput) (Result, error) {
	store := knowledge.New(e.maxChars)
	called := &orderedSet{seen: map[string]bool{}}

	for iter := 1; iter <= e.maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			return Result{Store: store, ToolsCalled: called.items, Iterations: iter - 1}, err
		}
		e.handleInterrupt(ctx)
		e.logKnowledge(iter, store)
		system := e.injectStickyNotes(in.System)
		user := buildTurnPrompt(in.Task, store, iter, e.maxIter, e.schemas, false)
		content, err := e.client.Chat(ctx, system, user, e.schemas)
		if err != nil {
			return Result{Store: store, ToolsCalled: called.items, Iterations: iter - 1}, err
		}
		e.logResponse(iter, content)

		calls := toolproto.Parse(content)
		if len(calls) == 0 {
			// No tool call: this is the model's final analysis.
			return Result{Content: strings.TrimSpace(content), ToolsCalled: called.items, Store: store, Iterations: iter - 1}, nil
		}

		if tools.VerboseEnabled() {
			fmt.Fprintf(os.Stderr, "[inspect] tool round %d: %d tool call(s)\n", iter, len(calls))
		}
		for _, c := range calls {
			called.add(c.Name)
			res, execErr := tools.Execute(ctx, c.Name, c.Args)
			ingest(store, c, res, execErr)
		}
	}

	// Tool budget exhausted: demand a final answer, advertising no tools.
	e.logKnowledge(e.maxIter+1, store)
	system := e.injectStickyNotes(in.System)
	user := buildTurnPrompt(in.Task, store, e.maxIter, e.maxIter, nil, true)
	content, err := e.client.Chat(ctx, system, user, nil)
	if err != nil {
		return Result{Store: store, ToolsCalled: called.items, Iterations: e.maxIter}, err
	}
	e.logResponse(e.maxIter+1, content)
	return Result{Content: strings.TrimSpace(content), ToolsCalled: called.items, Store: store, Iterations: e.maxIter}, nil
}

// logKnowledge prints the accumulated fact tree before a turn when -v or --debug
// is set. Under --debug the proxy used to dump the conversation; these stages
// now bypass the proxy, so the engine surfaces its own context here. Output goes
// to stderr alongside the per-tool progress logging.
func (e *Engine) logKnowledge(iter int, store *knowledge.Store) {
	if !tools.VerboseEnabled() && !tools.DebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "\n\033[1m===== inspect: fact tree before round %d =====\033[0m\n", iter)
	if store.Empty() {
		fmt.Fprintln(os.Stderr, "(empty — no tools run yet)")
	} else {
		fmt.Fprintln(os.Stderr, store.Render())
	}
	fmt.Fprintln(os.Stderr, strings.Repeat("=", 46))
}

// logResponse prints the raw LLM reply for a turn under --debug, so an operator
// sees exactly what the model produced (tool calls and/or final prose).
func (e *Engine) logResponse(iter int, content string) {
	if !tools.DebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "\n----- inspect: LLM response (round %d) -----\n%s\n%s\n",
		iter, strings.TrimSpace(content), strings.Repeat("-", 44))
}

// buildTurnPrompt assembles the single user message for one turn: the objective,
// everything learned so far, and a next-step instruction. When final is true the
// model is told to stop calling tools and write its answer.
func buildTurnPrompt(task string, store *knowledge.Store, iter, maxIter int, schemas []tools.Schema, final bool) string {
	var b strings.Builder

	b.WriteString("## Your objective\n\n")
	b.WriteString(strings.TrimSpace(task))
	b.WriteString("\n\n")

	b.WriteString("## What you have learned so far\n\n")
	if store.Empty() {
		b.WriteString("Nothing yet — you have not run any tools.\n\n")
	} else {
		b.WriteString(store.Render())
		b.WriteString("\n\n")
	}

	if final {
		b.WriteString("## Final step\n\n")
		b.WriteString("You have used your entire tool budget. Do NOT request any more tools. " +
			"Using ONLY the facts above, write your final analysis now in the required format.")
		return b.String()
	}

	fmt.Fprintf(&b, "## Next step (tool round %d of %d)\n\n", iter, maxIter)
	b.WriteString("Decide one of two things:\n")
	b.WriteString("1. If the facts above are sufficient, write your final analysis now in the required format and call no tools.\n")
	b.WriteString("2. Otherwise, call one or more tools to gather the specific evidence you still need.\n\n")
	b.WriteString("To call a tool, emit it on its own line as:\n")
	b.WriteString("`TOOL_CALL{\"name\":\"<tool>\",\"args\":{...}}`\n\n")
	b.WriteString("Do NOT re-request anything already shown above — those results will not change. ")
	b.WriteString("Build on what you know; investigate files and symbols you have not yet examined.\n")
	if len(schemas) > 0 {
		b.WriteString("\nAvailable tools:\n")
		for _, s := range schemas {
			fmt.Fprintf(&b, "- %s — %s\n", s.Name, firstSentence(s.Description))
		}
	}
	return b.String()
}

func firstSentence(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i+1]
	}
	return s
}

// handleInterrupt checks for a queued operator message between turns. A blank
// line (operator pressed Enter) is a pause signal: we prompt for the real
// message and block until one arrives. Any message becomes a sticky note,
// re-injected into the system prompt for the rest of the run.
func (e *Engine) handleInterrupt(ctx context.Context) {
	if e.interrupt == nil {
		return
	}
	msg, ok := e.interrupt.TakeNonBlocking()
	if !ok {
		return
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		fmt.Fprint(os.Stdout, "\n\033[1;97;41m INSPECT PAUSED \033[0m\nEnter guidance (or press Enter to resume): ")
		reply, ok := e.interrupt.ReadLine(ctx)
		if !ok || strings.TrimSpace(reply) == "" {
			fmt.Fprint(os.Stdout, "[resuming]\n\n")
			return
		}
		msg = strings.TrimSpace(reply)
	}
	fmt.Fprintf(os.Stdout, "\n\033[1;97;41m OPERATOR GUIDANCE ADDED \033[0m\n%s\n\n", msg)
	e.interrupt.AddStickyNote(msg)
}

// injectStickyNotes appends accumulated operator guidance to the system prompt
// so it persists across every subsequent turn.
func (e *Engine) injectStickyNotes(system string) string {
	if e.interrupt == nil {
		return system
	}
	notes := e.interrupt.StickyNotes()
	if len(notes) == 0 {
		return system
	}
	var b strings.Builder
	b.WriteString(system)
	b.WriteString("\n\n## Operator guidance (follow this)\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	return b.String()
}

// orderedSet records distinct tool names in first-seen order.
type orderedSet struct {
	seen  map[string]bool
	items []string
}

func (o *orderedSet) add(name string) {
	if o.seen[name] {
		return
	}
	o.seen[name] = true
	o.items = append(o.items, name)
}
