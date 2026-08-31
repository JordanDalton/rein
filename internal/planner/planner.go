// Package planner turns an intent plus a capability spec into the next argv to
// run. It is the only part of rein that talks to a model.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Backend is a single-shot completion. Keeping it this narrow means the rein
// works against the Claude API, the local `claude` CLI, or a fake in tests
// without any of them leaking into the loop.
type Backend interface {
	Complete(ctx context.Context, system, user string) (string, error)
	Name() string
}

// Action is what the planner wants to do next.
type Action string

const (
	// ActionRun proposes a command to execute.
	ActionRun Action = "run"
	// ActionAnswer means the intent has been satisfied.
	ActionAnswer Action = "answer"
	// ActionAsk means the planner needs something only the user knows.
	ActionAsk Action = "ask"
)

// Plan is one decision from the model.
type Plan struct {
	Reasoning string   `json:"reasoning"`
	Action    Action   `json:"action"`
	Argv      []string `json:"argv,omitempty"`
	Risk      string   `json:"risk,omitempty"`
	Purpose   string   `json:"purpose,omitempty"`
	// Consequence is the plain-language warning shown at the approval gate.
	// The gate is only a real check if the person reading it can tell what
	// they are agreeing to — which is not the same as being able to read the
	// command.
	Consequence string `json:"consequence,omitempty"`
	Answer      string `json:"answer,omitempty"`
	Question    string `json:"question,omitempty"`
}

// Step is one entry of execution history fed back to the model.
type Step struct {
	Argv     []string
	Purpose  string
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
	Note     string // for non-execution events: a user answer, a denied command
}

const systemPrompt = `You drive a single command-line tool on behalf of a user. You are given that tool's help text and the user's intent, and you choose one command at a time.

Reply with a single JSON object and nothing else. No prose, no markdown fences.

Schema:
{
  "reasoning": "one or two sentences on why this is the next step",
  "action": "run" | "answer" | "ask",
  "argv": ["tool", "subcommand", "..."],   // required when action is "run"
  "risk": "safe" | "caution" | "danger",   // required when action is "run"
  "purpose": "short human-readable description of what this command does",
  "consequence": "plain-language warning; required when risk is caution or danger",
  "answer": "the final answer to the user",  // required when action is "answer"
  "question": "what you need from the user"  // required when action is "ask"
}

Rules:
- argv[0] MUST be the wrapped tool. You cannot run any other program.
- argv is exec'd directly, never through a shell. Pipes, redirects, globs,
  $VARS, &&, and ; do not work — put each token in its own array element and
  use the tool's own flags to filter or format.
- Prefer flags that produce machine-readable output (--json, -o json, --format)
  when the tool offers them, and prefer narrow queries over dumping everything.
- Classify risk honestly. "safe" means the command only reads state. "caution"
  means it changes something recoverable. "danger" means it deletes, overwrites,
  or is otherwise hard to undo. When unsure, pick the higher level.
- Whenever risk is "caution" or "danger", write "consequence": one or two plain
  sentences that someone who cannot read the command would understand. Say what
  will change, in terms of their work rather than the tool's vocabulary, and say
  whether it can be undone — write "This cannot be undone." outright when that
  is true. Do not restate the command, name flags, or explain the syntax. The
  person approving this may not be able to read shell; that sentence is the only
  thing standing between them and the result.
- Only propose commands and flags that appear in the help text you were given.
  If the tool cannot do what was asked, say so with action "answer".
- Use "ask" only for information you genuinely cannot discover by running a
  read-only command first.
- When you have enough to respond, use "answer" and answer the actual question.
  Cite the concrete values you saw; do not hedge or summarise vaguely.`

// SystemPrompt returns the planner's system prompt.
func SystemPrompt() string { return systemPrompt }

// BuildUser renders the per-turn user message: capabilities, intent, history.
func BuildUser(tool, digest, intent string, steps []Step) string {
	var b strings.Builder
	b.WriteString("=== TOOL CAPABILITIES ===\n")
	b.WriteString(digest)
	fmt.Fprintf(&b, "\n\n=== USER INTENT ===\n%s\n", intent)

	if len(steps) == 0 {
		fmt.Fprintf(&b, "\nNothing has been run yet. Choose the first %s command.\n", tool)
	} else {
		b.WriteString("\n=== HISTORY ===\n")
		for i, s := range steps {
			if s.Note != "" {
				fmt.Fprintf(&b, "\n[step %d] %s\n", i+1, s.Note)
				continue
			}
			fmt.Fprintf(&b, "\n[step %d] $ %s\n", i+1, strings.Join(s.Argv, " "))
			if s.Purpose != "" {
				fmt.Fprintf(&b, "purpose: %s\n", s.Purpose)
			}
			if s.TimedOut {
				b.WriteString("result: TIMED OUT\n")
			} else {
				fmt.Fprintf(&b, "exit: %d\n", s.ExitCode)
			}
			if s.Stdout != "" {
				fmt.Fprintf(&b, "stdout:\n%s\n", s.Stdout)
			}
			if s.Stderr != "" {
				fmt.Fprintf(&b, "stderr:\n%s\n", s.Stderr)
			}
		}
		b.WriteString("\nDecide the next step.\n")
	}
	return b.String()
}

// Next asks the backend for one decision.
func Next(ctx context.Context, b Backend, tool, digest, intent string, steps []Step) (*Plan, error) {
	raw, err := b.Complete(ctx, systemPrompt, BuildUser(tool, digest, intent, steps))
	if err != nil {
		return nil, err
	}
	return ParsePlan(raw)
}

// ParsePlan pulls a Plan out of a model response, tolerating the fenced or
// chatty replies that models occasionally produce despite instructions.
func ParsePlan(raw string) (*Plan, error) {
	body := extractJSON(raw)
	if body == "" {
		return nil, fmt.Errorf("no JSON object in model response: %s", clip(raw, 300))
	}
	var p Plan
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		return nil, fmt.Errorf("malformed plan JSON: %w (got %s)", err, clip(body, 300))
	}
	// Weaker models reliably produce valid JSON (the API can enforce that) but
	// not necessarily *this* JSON. Quote what came back: when the schema is the
	// thing that broke, the payload is the only useful diagnostic.
	switch p.Action {
	case ActionRun:
		if len(p.Argv) == 0 {
			return nil, fmt.Errorf(`plan action "run" has an empty argv: %s`, clip(body, 400))
		}
	case ActionAnswer, ActionAsk:
	default:
		return nil, fmt.Errorf("model did not follow the plan schema: action was %q, want run/answer/ask: %s",
			p.Action, clip(body, 400))
	}
	return &p, nil
}

// extractJSON returns the outermost balanced {...} span, ignoring braces inside
// JSON strings.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// nothing
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
