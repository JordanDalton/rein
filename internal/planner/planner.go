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

// BuildSystem renders the full system prompt: the instructions plus the tool's
// capability digest. The digest lives here rather than in the user message
// because it is byte-identical on every step of a run — backends that cache
// the system prefix (the Claude API explicitly, most hosted endpoints
// implicitly) then reprocess only the turn itself instead of the whole map.
func BuildSystem(digest string) string {
	return systemPrompt + "\n\n=== TOOL CAPABILITIES ===\n" + digest
}

// keepFullSteps is how many recent steps keep their command output untrimmed
// when the transcript is rebuilt each turn. Older output has served its
// purpose — the model already acted on it — and resending all of it makes
// every later step slower than the one before.
const keepFullSteps = 3

// trimOutputTo bounds the output of older steps. Head and tail survive because
// that is where identifiers and error summaries usually live.
const trimOutputTo = 700

// BuildUser renders the per-turn user message: intent plus execution history.
func BuildUser(tool, intent string, steps []Step) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== USER INTENT ===\n%s\n", intent)

	if len(steps) == 0 {
		fmt.Fprintf(&b, "\nNothing has been run yet. Choose the first %s command.\n", tool)
		return b.String()
	}
	b.WriteString("\n=== HISTORY ===\n")
	writeSteps(&b, steps, 0, len(steps)-keepFullSteps)
	b.WriteString("\nDecide the next step.\n")
	return b.String()
}

// writeSteps renders steps into b. base is the index of steps[0] in the whole
// run, so numbering stays stable across delta messages; output of steps before
// index trimBefore (relative to the run) is clipped.
func writeSteps(b *strings.Builder, steps []Step, base, trimBefore int) {
	for i, s := range steps {
		n := base + i
		if s.Note != "" {
			fmt.Fprintf(b, "\n[step %d] %s\n", n+1, s.Note)
			continue
		}
		fmt.Fprintf(b, "\n[step %d] $ %s\n", n+1, strings.Join(s.Argv, " "))
		if s.Purpose != "" {
			fmt.Fprintf(b, "purpose: %s\n", s.Purpose)
		}
		if s.TimedOut {
			b.WriteString("result: TIMED OUT\n")
		} else {
			fmt.Fprintf(b, "exit: %d\n", s.ExitCode)
		}
		stdout, stderr := s.Stdout, s.Stderr
		if n < trimBefore {
			stdout, stderr = clipMiddle(stdout, trimOutputTo), clipMiddle(stderr, trimOutputTo)
		}
		if stdout != "" {
			fmt.Fprintf(b, "stdout:\n%s\n", stdout)
		}
		if stderr != "" {
			fmt.Fprintf(b, "stderr:\n%s\n", stderr)
		}
	}
}

// Sessional is an optional Backend capability: the backend holds its
// conversation open between calls, so the caller sends only what is new
// instead of rebuilding the whole transcript every step.
type Sessional interface {
	Backend
	// Send delivers one message into the live conversation (starting it with
	// the given system prompt if needed) and returns the reply.
	Send(ctx context.Context, system, message string) (string, error)
}

// Session is one run's conversation with a backend. For a plain Backend each
// Next rebuilds the full context; for a Sessional one it sends only the steps
// the backend has not yet seen.
type Session struct {
	b      Backend
	tool   string
	digest string
	intent string
	sent   int  // steps already delivered to a Sessional backend
	opened bool // a Sessional conversation is live
}

// NewSession binds a backend to one run's tool, capability digest, and intent.
func NewSession(b Backend, tool, digest, intent string) *Session {
	return &Session{b: b, tool: tool, digest: digest, intent: intent}
}

// Next asks the backend for one decision given the history so far.
func (s *Session) Next(ctx context.Context, steps []Step) (*Plan, error) {
	system := BuildSystem(s.digest)

	sb, ok := s.b.(Sessional)
	if !ok {
		raw, err := s.b.Complete(ctx, system, BuildUser(s.tool, s.intent, steps))
		if err != nil {
			return nil, err
		}
		return ParsePlan(raw)
	}

	var msg string
	if !s.opened {
		msg = BuildUser(s.tool, s.intent, steps)
	} else {
		var b strings.Builder
		writeSteps(&b, steps[s.sent:], s.sent, 0)
		b.WriteString("\nDecide the next step.\n")
		msg = b.String()
	}

	raw, err := sb.Send(ctx, system, msg)
	if err != nil && s.opened {
		// The session may simply have died (process exit, dropped connection).
		// One retry with the full transcript rebuilds the lost context.
		raw, err = sb.Send(ctx, system, BuildUser(s.tool, s.intent, steps))
	}
	if err != nil {
		return nil, err
	}
	s.opened = true
	s.sent = len(steps)
	return ParsePlan(raw)
}

// clipMiddle keeps the head and tail of s within a budget of n bytes, cutting
// the middle. Unlike clip, the tail survives — with command output that is
// where the error summary usually is.
func clipMiddle(s string, n int) string {
	if len(s) <= n {
		return s
	}
	half := n / 2
	return fmt.Sprintf("%s\n… [%d bytes trimmed] …\n%s", s[:half], len(s)-n, s[len(s)-half:])
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
	// Weaker models sometimes drop the action discriminator while filling in
	// exactly one payload field. That is unambiguous, so repair it rather than
	// abort the run.
	if p.Action == "" {
		switch {
		case len(p.Argv) > 0 && p.Answer == "" && p.Question == "":
			p.Action = ActionRun
		case p.Answer != "" && len(p.Argv) == 0 && p.Question == "":
			p.Action = ActionAnswer
		case p.Question != "" && len(p.Argv) == 0 && p.Answer == "":
			p.Action = ActionAsk
		}
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
