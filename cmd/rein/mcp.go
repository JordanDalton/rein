package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jordandalton/rein/internal/loop"
	"github.com/jordandalton/rein/internal/mcp"
	"github.com/jordandalton/rein/internal/planner"
	"github.com/jordandalton/rein/internal/spec"
)

// cmdMCP serves rein over the Model Context Protocol on stdio, so a coding
// agent can hand an unfamiliar CLI to rein instead of guessing at its flags.
//
// Stdout is the transport from here on: nothing else may write to it. Runs
// are headless — there is no terminal to approve a command or answer a
// question — so the loop fails closed at the gate and the stop is reported
// back to the caller as an outcome it can act on.
func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(usage) }
	steps := fs.Int("steps", 8, "")
	yes := fs.Bool("yes", false, "")
	auto := fs.Bool("auto", false, "")
	backend := fs.String("backend", "claude-cli", "")
	model := fs.String("model", "", "")
	baseURL := fs.String("base-url", "", "")
	keyEnv := fs.String("api-key-env", "", "")
	timeout := fs.Duration("timeout", 60*time.Second, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: rein mcp [--yes|--auto] [--backend NAME] [--model NAME]")
	}

	// Fail early on a misconfigured backend rather than on the first call.
	if _, err := makeBackend(*backend, *model, *baseURL, *keyEnv); err != nil {
		return err
	}

	d := &mcpDeps{
		ceiling: loop.ApproveSafe,
		steps:   *steps,
		timeout: *timeout,
		newSpec: func(ctx context.Context, tool string, refresh bool) (*spec.Spec, error) {
			return ensureSpec(ctx, tool, refresh, spec.Options{})
		},
		newBackend: func() (planner.Backend, error) {
			return makeBackend(*backend, *model, *baseURL, *keyEnv)
		},
	}
	if *yes {
		d.ceiling = loop.ApproveCaution
	}
	if *auto {
		d.ceiling = loop.ApproveAll
	}

	srv := mcp.New("rein", version(), os.Stdin, os.Stdout)
	srv.Instructions = mcpInstructions
	for _, t := range d.tools() {
		srv.Add(t)
	}
	fmt.Fprintf(os.Stderr, "rein mcp: serving on stdio · backend %s · approval ceiling %s\n",
		*backend, approvalName(d.ceiling))
	return srv.Serve(ctx)
}

const mcpInstructions = `rein drives a command-line tool from a plain-language intent: it learns the tool's commands and flags once, then plans, runs and reads commands until the intent is satisfied. Use rein_in when you need something done with a CLI you do not know well, or one that is internal to this machine. rein never runs a shell and only ever executes the one binary named in the call.`

// mcpDeps is what the MCP tools need from the rest of rein, factored so a
// test can substitute a scripted backend and a canned spec.
type mcpDeps struct {
	ceiling    loop.Approval
	steps      int
	timeout    time.Duration
	newSpec    func(ctx context.Context, tool string, refresh bool) (*spec.Spec, error)
	newBackend func() (planner.Backend, error)
}

func (d *mcpDeps) tools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name: "rein_in",
			Description: "Drive a command-line tool to satisfy an intent. rein learns the tool's " +
				"capabilities on first use, then plans and runs commands step by step, reading " +
				"each result, until it can answer. Read-only commands run unattended; mutating " +
				"and destructive ones need approval (see the approval argument). Returns the " +
				"answer plus a transcript of every command run.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "tool": {
      "type": "string",
      "description": "The CLI to drive, as it is typed: \"git\", \"kubectl\", \"gh\", \"acme-deploy\". Must be on PATH."
    },
    "intent": {
      "type": "string",
      "description": "What you want, in plain language. Include anything the tool would need to know (names, paths, environments)."
    },
    "approval": {
      "type": "string",
      "enum": ["safe", "yes", "auto"],
      "description": "How much may run without a human. \"safe\" (default): read-only commands only; the run stops and reports if a mutating command is needed. \"yes\": also run recoverable mutations. \"auto\": run everything, including destructive commands. The server caps this; a request above its ceiling is refused, and you should ask the user rather than retry."
    },
    "refresh": {
      "type": "boolean",
      "description": "Re-learn the tool's capabilities before running, e.g. after it was upgraded."
    }
  },
  "required": ["tool", "intent"]
}`),
			Handler: d.runIn,
		},
		{
			Name: "rein_list",
			Description: "List the tools rein has already learned. Any tool on PATH can be used with " +
				"rein_in whether or not it appears here; unlisted tools just take a few seconds " +
				"longer on first use.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Handler:     d.list,
		},
		{
			Name: "rein_spec",
			Description: "Show what rein knows about a tool: its subcommands and their summaries. " +
				"Learns the tool first if needed. Useful to check whether a tool can do something " +
				"before asking rein_in to do it.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "tool": {"type": "string", "description": "The CLI, as typed."},
    "refresh": {"type": "boolean", "description": "Re-learn even if a cached map exists."}
  },
  "required": ["tool"]
}`),
			Handler: d.showSpec,
		},
	}
}

func (d *mcpDeps) runIn(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Tool     string `json:"tool"`
		Intent   string `json:"intent"`
		Approval string `json:"approval"`
		Refresh  bool   `json:"refresh"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	args.Tool = strings.TrimSpace(args.Tool)
	args.Intent = strings.TrimSpace(args.Intent)
	if args.Tool == "" || args.Intent == "" {
		return "", errors.New("both tool and intent are required")
	}
	if strings.ContainsAny(args.Tool, " \t\n") {
		return "", fmt.Errorf("tool must be a single program name, got %q; put the rest in the intent", args.Tool)
	}

	approval, err := parseApproval(args.Approval)
	if err != nil {
		return "", err
	}
	if approval > d.ceiling {
		return "", fmt.Errorf("approval %q is above this server's ceiling of %q; "+
			"it was started without --%s. Ask the user, who can restart it with `rein mcp --%s` "+
			"or run `rein in --%s %s %q` themselves",
			approvalName(approval), approvalName(d.ceiling), approvalFlag(approval),
			approvalFlag(approval), approvalFlag(approval), args.Tool, args.Intent)
	}

	sp, err := d.newSpec(ctx, args.Tool, args.Refresh)
	if err != nil {
		return "", err
	}
	be, err := d.newBackend()
	if err != nil {
		return "", err
	}
	if closer, ok := be.(io.Closer); ok {
		defer closer.Close()
	}

	var transcript strings.Builder
	answer, err := loop.Run(ctx, loop.Config{
		Spec:     sp,
		Backend:  be,
		Intent:   args.Intent,
		MaxSteps: d.steps,
		Approval: approval,
		Timeout:  d.timeout,
		Out:      &transcript,
		In:       strings.NewReader(""), // headless: every prompt hits EOF and fails closed
	})
	return formatOutcome(answer, err, approval, d.ceiling, stripANSI(transcript.String()))
}

// formatOutcome renders a finished run for the calling model. A stop at the
// gate or at a question is a legitimate outcome with a next step, not a
// failure, so those come back as ordinary text.
func formatOutcome(answer string, err error, approval, ceiling loop.Approval, transcript string) (string, error) {
	var b strings.Builder
	var needs *loop.NeedsInputError
	switch {
	case err == nil:
		b.WriteString(answer)
	case errors.As(err, &needs):
		fmt.Fprintf(&b, "status: needs-input\n\nrein needs an answer before it can continue:\n\n  %s\n\n"+
			"Answer it if you can, otherwise ask the user; then call rein_in again with the "+
			"answer included in the intent.", needs.Question)
	case errors.Is(err, loop.ErrNoTerminal):
		fmt.Fprintf(&b, "status: needs-approval\n\nrein stopped before the last command in the transcript "+
			"below: it is not read-only, and this run was granted approval %q. Nothing was executed "+
			"for that step.\n\n", approvalName(approval))
		if approval < ceiling {
			fmt.Fprintf(&b, "To run it, ask the user, then call rein_in again with approval %q.",
				approvalName(min(ceiling, approval+1)))
		} else {
			fmt.Fprintf(&b, "This server's ceiling is %q, so it cannot run here. Ask the user; they can "+
				"restart the server with `rein mcp --%s` or run the command themselves.",
				approvalName(ceiling), approvalFlag(ceiling+1))
		}
	default:
		return "", fmt.Errorf("%v\n\n--- transcript ---\n%s", err, strings.TrimSpace(transcript))
	}
	if t := strings.TrimSpace(transcript); t != "" {
		fmt.Fprintf(&b, "\n\n--- transcript ---\n%s", t)
	}
	return b.String(), nil
}

func (d *mcpDeps) list(context.Context, json.RawMessage) (string, error) {
	specs, err := cachedSpecs()
	if err != nil {
		return "", err
	}
	if len(specs) == 0 {
		return "no tools learned yet; rein_in learns a tool on first use", nil
	}
	var b strings.Builder
	for _, sp := range specs {
		b.WriteString(describeSpec(sp))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (d *mcpDeps) showSpec(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Tool    string `json:"tool"`
		Refresh bool   `json:"refresh"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	if strings.TrimSpace(args.Tool) == "" {
		return "", errors.New("tool is required")
	}
	sp, err := d.newSpec(ctx, strings.TrimSpace(args.Tool), args.Refresh)
	if err != nil {
		return "", err
	}
	// Subcommand list and root help only; per-command help would swamp the
	// caller's context for a question that is usually "can this tool X?".
	return sp.Digest(8000), nil
}

func parseApproval(s string) (loop.Approval, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "safe":
		return loop.ApproveSafe, nil
	case "yes":
		return loop.ApproveCaution, nil
	case "auto":
		return loop.ApproveAll, nil
	}
	return 0, fmt.Errorf("approval must be \"safe\", \"yes\" or \"auto\", got %q", s)
}

func approvalName(a loop.Approval) string {
	switch a {
	case loop.ApproveCaution:
		return "yes"
	case loop.ApproveAll:
		return "auto"
	}
	return "safe"
}

// approvalFlag is the `rein in` / `rein mcp` flag that grants a level.
func approvalFlag(a loop.Approval) string {
	if a >= loop.ApproveAll {
		return "auto"
	}
	return "yes"
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }
