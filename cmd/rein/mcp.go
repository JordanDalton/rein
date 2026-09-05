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
	"github.com/jordandalton/rein/internal/policy"
	"github.com/jordandalton/rein/internal/risk"
	"github.com/jordandalton/rein/internal/runner"
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
	agent := fs.String("agent", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: rein mcp [--yes|--auto] [--backend NAME] [--model NAME]")
	}
	if *agent != "" {
		if _, err := registeredAgent(*agent); err != nil {
			return err
		}
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
	if *agent != "" {
		d.caller = *agent
	}
	if *yes {
		d.ceiling = loop.ApproveCaution
	}
	if *auto {
		d.ceiling = loop.ApproveAll
	}
	if d.caller != "" {
		g, err := newMCPGoverned(d.caller)
		if err != nil {
			return err
		}
		if _, err := g.bundle(ctx); err != nil {
			return err
		}
	}

	srv := mcp.New("rein", version(), os.Stdin, os.Stdout)
	srv.Instructions = mcpInstructions
	for _, t := range d.tools() {
		srv.Add(t)
	}
	fmt.Fprintf(os.Stderr, "rein mcp: serving on stdio · backend %s · approval ceiling %s",
		*backend, approvalName(d.ceiling))
	if d.caller != "" {
		fmt.Fprintf(os.Stderr, " · caller %s", d.caller)
	}
	fmt.Fprintln(os.Stderr)
	return srv.Serve(ctx)
}

const mcpInstructions = `Rein is the execution governor for this Claude Code session. All command execution must go through the Rein MCP tools, especially rein_in; never use Bash, a shell, direct CLI invocation, or another execution tool as a fallback. If Rein reports that an operation needs approval, needs input, is blocked, or is slow, relay that result to the user and wait or ask for the required next step. Do not decide that a command is harmless and bypass Rein. rein_in drives one named CLI from a plain-language intent, learns its commands and flags, applies organization policy, coordinates Cloud approval, executes without a shell, and returns the transcript. Use rein_list to inspect learned tools and rein_spec to inspect a tool's capabilities.`

// mcpDeps is what the MCP tools need from the rest of rein, factored so a
// test can substitute a scripted backend and a canned spec.
type mcpDeps struct {
	caller     string
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
				"capabilities on first use in standalone mode; registered-agent mode requires a trusted cached spec. It plans and runs commands step by step, reading " +
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
			Name:        "rein_list",
			Description: "List learned tools. Registered-agent mode only uses cached specs; a trusted operator must learn new tools outside the MCP session.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Handler:     d.list,
		},
		{
			Name: "rein_spec",
			Description: "Show what rein knows about a tool: its subcommands and their summaries. " +
				"Standalone mode learns the tool if needed; registered-agent mode never runs discovery. Useful to check whether a tool can do something " +
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
	// Policies published in the dashboard become effective without requiring
	// the user to restart MCP or run a separate sync command.
	if d.caller == "" {
		refreshCloudPolicy(ctx, 2*time.Minute)
	}
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

	var governed *mcpGoverned
	if d.caller != "" {
		governed, err = newMCPGoverned(d.caller)
		if err != nil {
			return "", err
		}
		governed.operation = map[string]any{"tool": args.Tool, "intent": args.Intent}
		bundle, err := governed.bundle(ctx)
		if bundle.Version > 0 {
			governed.operation["policy_version"] = bundle.Version
		}
		if err != nil {
			return "", governed.recordBlock(ctx, "policy_preflight", err)
		}
	}
	sp, err := d.toolSpec(ctx, args.Tool, args.Refresh)
	if err != nil {
		if governed != nil {
			return "", governed.recordBlock(ctx, "spec_preflight", err)
		}
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
	cfg := loop.Config{
		Spec:     sp,
		Backend:  be,
		Intent:   args.Intent,
		MaxSteps: d.steps,
		Approval: approval,
		Timeout:  d.timeout,
		Policy: func(argv []string, level risk.Level) error {
			return policy.CheckIntent(d.caller, args.Intent, argv, level)
		},
		RequireApproval: func(argv []string, level risk.Level) bool {
			return d.caller != "" && policy.RequiresApproval(d.caller, args.Intent, argv, level)
		},
		ApprovalCheck: func(argv []string) bool {
			return d.caller != "" && cloudApprovalGranted(ctx, d.caller, args.Tool, args.Intent, argv)
		},
		Out: &transcript,
		In:  strings.NewReader(""), // headless: every prompt hits EOF and fails closed
	}
	if governed != nil {
		cfg.Policy, cfg.RequireApproval, cfg.ApprovalCheck = nil, nil, nil
		cfg.Authorize = func(argv []string, level risk.Level) error {
			if err := governed.authorize(ctx, args.Tool, args.Intent, argv, level); err != nil {
				return governed.recordBlock(ctx, "authorization", err)
			}
			return nil
		}
		cfg.BeforeExecute = func(argv []string) error {
			if err := governed.audit(ctx, "execution_started", nil, nil); err != nil {
				return governed.recordBlock(ctx, "before_execution", err)
			}
			return nil
		}
		cfg.AfterExecute = func(argv []string, result *runner.Result, runErr error) error {
			return governed.audit(ctx, "execution_completed", result, runErr)
		}
	}
	answer, err := loop.Run(ctx, cfg)
	return formatOutcome(answer, err, approval, d.ceiling, stripANSI(transcript.String()))
}

func attemptedCommand(transcript string) string {
	for _, line := range strings.Split(stripANSI(transcript), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "$ "); i >= 0 {
			return strings.TrimSpace(line[i+2:])
		}
	}
	return ""
}

func auditEvent(err error) string {
	if err == nil {
		return "executed"
	}
	var needs *loop.NeedsInputError
	switch {
	case errors.As(err, &needs):
		return "needs_input"
	case errors.Is(err, loop.ErrNoTerminal):
		return "approval_required"
	case strings.Contains(strings.ToLower(err.Error()), "blocked by policy") ||
		strings.Contains(strings.ToLower(err.Error()), "not permitted"):
		return "policy_denied"
	default:
		return "failed"
	}
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
	sp, err := d.toolSpec(ctx, strings.TrimSpace(args.Tool), args.Refresh)
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
