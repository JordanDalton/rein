// Package loop runs the agentic cycle: plan, gate, execute, observe, repeat.
package loop

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jordandalton/rein/internal/planner"
	"github.com/jordandalton/rein/internal/risk"
	"github.com/jordandalton/rein/internal/runner"
	"github.com/jordandalton/rein/internal/spec"
)

// Approval says how much the loop may do without asking.
type Approval int

const (
	// ApproveSafe runs read-only commands unattended and asks about everything
	// else. This is the default.
	ApproveSafe Approval = iota
	// ApproveCaution additionally runs recoverable mutations unattended.
	// Destructive commands still stop for a human.
	ApproveCaution
	// ApproveAll asks about nothing. For sandboxes and CI, not for laptops.
	ApproveAll
)

// Config is one `rein in`.
type Config struct {
	Spec     *spec.Spec
	Backend  planner.Backend
	Intent   string
	MaxSteps int
	Approval Approval
	DryRun   bool // plan and print, never execute
	Timeout  time.Duration
	Verbose  bool
	WorkDir  string
	Out      io.Writer
	In       io.Reader // approval input; defaults to /dev/tty, then stdin
	// Policy receives the validated argv immediately before approval/execution.
	// A non-nil error blocks the command without ever invoking the wrapped CLI.
	Policy          func([]string, risk.Level) error
	ApprovalCheck   func([]string) bool
	RequireApproval func([]string, risk.Level) bool
	// Authorize replaces the interactive gate for externally governed runs.
	Authorize     func([]string, risk.Level) error
	BeforeExecute func([]string) error
	AfterExecute  func([]string, *runner.Result, error) error
}

// ErrDenied is returned when the user stops the run at an approval prompt.
var ErrDenied = errors.New("stopped by user")

// ErrNoTerminal is wrapped by the error returned when a command needs a
// human's approval and there is no one to ask. Headless callers (the MCP
// server) match on it to explain the stop in their own terms.
var ErrNoTerminal = errors.New("no terminal available to confirm")

// NeedsInputError is returned when the planner asks the user a question and
// the run has no way to relay it. The question is kept so a headless caller
// can hand it back to whoever started the run.
type NeedsInputError struct{ Question string }

func (e *NeedsInputError) Error() string {
	return "the planner needs an answer to continue: " + e.Question
}

type gate struct {
	in  *bufio.Reader
	out io.Writer
}

// Run executes the loop and returns the model's final answer.
func Run(ctx context.Context, cfg Config) (string, error) {
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 8
	}
	g := &gate{in: bufio.NewReader(approvalInput(cfg.In)), out: cfg.Out}

	digest := cfg.Spec.Digest(24000)
	logDir := filepath.Join(spec.Home(), "runs", time.Now().Format("20060102-150405"))

	sess := planner.NewSession(cfg.Backend, cfg.Spec.Tool, digest, cfg.Intent)
	sess.Reasoning = cfg.Verbose
	var steps []planner.Step

	for i := 1; i <= cfg.MaxSteps; i++ {
		// MaxSteps is a budget, not a plan — printing "1/8" reads as a promise
		// of eight steps, so the cap only appears once it is nearly spent.
		if cfg.MaxSteps-i < 2 {
			fmt.Fprintf(cfg.Out, "\n\033[2m── step %d · %d step(s) left in budget · thinking…\033[0m\n",
				i, cfg.MaxSteps-i+1)
		} else {
			fmt.Fprintf(cfg.Out, "\n\033[2m── step %d · thinking…\033[0m\n", i)
		}

		plan, err := sess.Next(ctx, steps)
		if err != nil {
			return "", fmt.Errorf("step %d: %w", i, err)
		}
		if cfg.Verbose && plan.Reasoning != "" {
			fmt.Fprintf(cfg.Out, "\033[2m%s\033[0m\n", plan.Reasoning)
		}

		switch plan.Action {
		case planner.ActionAnswer:
			return plan.Answer, nil

		case planner.ActionAsk:
			fmt.Fprintf(cfg.Out, "\n\033[1m?\033[0m %s\n", plan.Question)
			ans, err := g.line("> ")
			if errors.Is(err, ErrNoTerminal) {
				return "", &NeedsInputError{Question: plan.Question}
			}
			if err != nil {
				return "", err
			}
			steps = append(steps, planner.Step{
				Note: fmt.Sprintf("asked the user %q; they answered %q", plan.Question, ans),
			})
			continue
		}

		// action == run
		if err := validate(plan.Argv, cfg.Spec); err != nil {
			steps = append(steps, planner.Step{
				Note: fmt.Sprintf("proposed %q, rejected: %v", strings.Join(plan.Argv, " "), err),
			})
			fmt.Fprintf(cfg.Out, "\033[31m✗ rejected:\033[0m %v\n", err)
			continue
		}

		// The model's own risk label and the static classifier are independent
		// checks; trusting the higher of the two means a model that
		// under-reports cannot talk its way past the gate.
		level := risk.Max(risk.Classify(plan.Argv), risk.Parse(plan.Risk))
		if cfg.Policy != nil {
			if err := cfg.Policy(plan.Argv, level); err != nil {
				fmt.Fprintf(cfg.Out, "\033[31m✗ blocked by policy:\033[0m %v\n", err)
				return "", err
			}
		}

		fmt.Fprintf(cfg.Out, "\n  \033[1m$ %s\033[0m\n", runner.Quote(plan.Argv))
		if plan.Purpose != "" {
			fmt.Fprintf(cfg.Out, "  \033[2m%s · %s\033[0m\n", plan.Purpose, badge(level))
		} else {
			fmt.Fprintf(cfg.Out, "  \033[2m%s\033[0m\n", badge(level))
		}

		// A model that called `cat .env` "safe" wrote no consequence, so the
		// gate would otherwise prompt with nothing to explain why.
		consequence := plan.Consequence
		if consequence == "" {
			if f := risk.SecretOperand(plan.Argv); f != "" {
				consequence = fmt.Sprintf("This reads %s, which usually holds credentials. "+
					"Whatever it prints is shown here, sent to the model, and kept in rein's run log. "+
					"Values that look like secrets are masked, but not everything sensitive looks like one.", f)
			}
		}
		if consequence != "" && level != risk.Safe {
			fmt.Fprint(cfg.Out, consequenceBlock(level, consequence))
		}

		if cfg.DryRun {
			fmt.Fprintln(cfg.Out, "  \033[2m(dry run — not executed)\033[0m")
			return fmt.Sprintf("Dry run: next command would be `%s`", runner.Quote(plan.Argv)), nil
		}

		var ok bool
		required := cfg.RequireApproval != nil && cfg.RequireApproval(plan.Argv, level)
		if cfg.Authorize != nil {
			err = cfg.Authorize(plan.Argv, level)
			ok = err == nil
		} else if cfg.ApprovalCheck != nil && cfg.ApprovalCheck(plan.Argv) {
			fmt.Fprintln(cfg.Out, "  approved by Rein Control policy review")
			ok = true
		} else {
			ok, err = g.approve(cfg, level, required)
		}
		if err != nil {
			return "", err
		}
		if !ok {
			steps = append(steps, planner.Step{
				Note: fmt.Sprintf("proposed %q; the user declined it. Try a different approach.",
					strings.Join(plan.Argv, " ")),
			})
			continue
		}

		if cfg.BeforeExecute != nil {
			if err := cfg.BeforeExecute(plan.Argv); err != nil {
				return "", fmt.Errorf("pre-execution audit failed; command not executed: %w", err)
			}
		}
		res, err := runner.Run(ctx, plan.Argv, runner.Options{Timeout: cfg.Timeout, LogDir: logDir, WorkDir: cfg.WorkDir})
		if cfg.AfterExecute != nil {
			if auditErr := cfg.AfterExecute(plan.Argv, res, err); auditErr != nil {
				return "", fmt.Errorf("post-execution audit failed; command may have executed, do not blindly retry: %w", auditErr)
			}
		}
		if err != nil {
			return "", err
		}

		status := fmt.Sprintf("exit %d", res.ExitCode)
		if res.TimedOut {
			status = "timed out"
		}
		fmt.Fprintf(cfg.Out, "  \033[2m%s in %s%s\033[0m\n", status,
			res.Duration.Round(time.Millisecond), outputNote(res))
		if body := strings.TrimSpace(res.Stdout + "\n" + res.Stderr); body != "" {
			if f := risk.SecretOperand(plan.Argv); f != "" {
				// The user asked a question about the file, not to see it.
				// The model gets the masked contents; the terminal gets a
				// summary, so a stray glance at the scrollback shows nothing.
				fmt.Fprintf(cfg.Out, "  \033[2m│ contents of %s withheld from the terminal (%d line(s), %d secret(s) masked); the model sees the masked version, as does the run log\033[0m\n",
					f, strings.Count(body, "\n")+1, res.Redacted)
			} else {
				fmt.Fprintln(cfg.Out, indent(body, "  │ "))
			}
		}

		steps = append(steps, planner.Step{
			Argv:     plan.Argv,
			Purpose:  plan.Purpose,
			ExitCode: res.ExitCode,
			Stdout:   res.Stdout,
			Stderr:   res.Stderr,
			TimedOut: res.TimedOut,
		})
	}

	return "", fmt.Errorf("gave up after %d steps without reaching an answer (raise --steps)", cfg.MaxSteps)
}

// validate is the hard boundary. Whatever the model returns, rein only
// ever runs the one binary it was pointed at.
func validate(argv []string, s *spec.Spec) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	head := filepath.Base(argv[0])
	if head != s.Tool && argv[0] != s.Binary {
		return fmt.Errorf("rein only runs %q, not %q", s.Tool, argv[0])
	}
	for _, a := range argv {
		if strings.ContainsAny(a, "\x00") {
			return errors.New("argument contains a null byte")
		}
	}
	return nil
}

func (g *gate) approve(cfg Config, level risk.Level, required bool) (bool, error) {
	switch {
	case !required && cfg.Approval == ApproveAll:
		return true, nil
	case !required && level == risk.Safe:
		return true, nil
	case !required && level == risk.Caution && cfg.Approval >= ApproveCaution:
		return true, nil
	}

	for {
		ans, err := g.line("  run this? [y]es / [n]o / [q]uit: ")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(ans)) {
		case "y", "yes":
			return true, nil
		case "n", "no", "":
			return false, nil
		case "q", "quit":
			return false, ErrDenied
		}
	}
}

func (g *gate) line(prompt string) (string, error) {
	fmt.Fprint(g.out, prompt)
	s, err := g.in.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%w; re-run interactively, or use --dry-run to preview (--auto skips every prompt, including destructive ones)", ErrNoTerminal)
		}
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// approvalInput prefers the controlling terminal so approvals still work when
// the intent was piped in on stdin.
func approvalInput(override io.Reader) io.Reader {
	if override != nil {
		return override
	}
	if tty, err := os.Open("/dev/tty"); err == nil {
		return tty
	}
	return os.Stdin
}

// consequenceBlock renders the plain-language warning that precedes the gate.
// It is deliberately the most prominent thing on screen: the command above it
// is precise but only legible to someone who already knows the tool.
func consequenceBlock(l risk.Level, text string) string {
	icon, colour := "!", "\033[33m"
	if l == risk.Danger {
		icon, colour = "!!", "\033[31m"
	}
	var b strings.Builder
	b.WriteString("\n")
	for i, line := range wrapText(text, 72) {
		if i == 0 {
			fmt.Fprintf(&b, "  %s%s\033[0m  %s\n", colour, icon, line)
		} else {
			fmt.Fprintf(&b, "  %*s  %s\n", len(icon), "", line)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// wrapText breaks text on spaces at the given width. Consequences are prose and
// a terminal will hard-wrap them mid-word otherwise.
func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	lines := []string{words[0]}
	for _, w := range words[1:] {
		last := len(lines) - 1
		if len(lines[last])+1+len(w) <= width {
			lines[last] += " " + w
			continue
		}
		lines = append(lines, w)
	}
	return lines
}

func badge(l risk.Level) string {
	switch l {
	case risk.Safe:
		return "\033[32mread-only\033[2m"
	case risk.Danger:
		return "\033[31mdestructive\033[2m"
	default:
		return "\033[33mmutating\033[2m"
	}
}

// outputNote explains what was done to the output before it was shown.
func outputNote(r *runner.Result) string {
	var notes []string
	if r.Redacted > 0 {
		notes = append(notes, fmt.Sprintf("%d secret(s) masked", r.Redacted))
	}
	if r.Elided {
		if r.LogPath != "" {
			notes = append(notes, "output elided, full log at "+r.LogPath)
		} else {
			notes = append(notes, "output elided")
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return " · " + strings.Join(notes, " · ")
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
