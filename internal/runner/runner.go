// Package runner executes a planned argv and turns its output into something a
// model can actually read.
//
// Two rules shape this package. First, no shell: the planner emits an argv
// array and we exec it directly, so quoting bugs cannot become command
// injection. Second, CLIs are hostile to programs — colour codes, pagers that
// block forever, forty thousand lines of logs — so the environment is
// sanitised going in and the output is elided coming out.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jordandalton/rein/internal/spec"
)

// Options tunes one execution.
type Options struct {
	Timeout  time.Duration // default 60s
	MaxLines int           // lines of each stream shown to the model (default 120)
	LogDir   string        // where full output is archived; "" disables
}

func (o *Options) withDefaults() {
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	if o.MaxLines == 0 {
		o.MaxLines = 120
	}
}

// Result is one executed step.
type Result struct {
	Argv     []string
	ExitCode int
	Stdout   string // elided, ANSI-stripped
	Stderr   string // elided, ANSI-stripped
	Elided   bool
	TimedOut bool
	Duration time.Duration
	LogPath  string // full untruncated output, when LogDir was set
}

// Env returns an environment that makes CLIs behave predictably for a
// non-interactive caller: no colour, no pager, a dumb terminal.
func Env() []string {
	return append(os.Environ(),
		"TERM=dumb",
		"NO_COLOR=1",
		"CLICOLOR=0",
		"CLICOLOR_FORCE=0",
		"COLUMNS=100",
		"PAGER=cat",
		"GIT_PAGER=cat",
		"GH_PAGER=cat",
		"AWS_PAGER=",
		"LESS=FRX",
		"GIT_TERMINAL_PROMPT=0",
		"DEBIAN_FRONTEND=noninteractive",
	)
}

// Run executes argv. argv[0] must already have been validated against the
// wrapped tool by the caller — this function does not decide what is allowed.
func Run(ctx context.Context, argv []string, opts Options) (*Result, error) {
	opts.withDefaults()
	if len(argv) == 0 {
		return nil, errors.New("empty argv")
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = Env()
	// Closed stdin, so a tool that decides to prompt fails fast instead of
	// hanging until the timeout.
	cmd.Stdin = bytes.NewReader(nil)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	start := time.Now()
	err := cmd.Run()
	res := &Result{Argv: argv, Duration: time.Since(start)}

	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		return nil, fmt.Errorf("could not run %q: %w", argv[0], err)
	}

	rawOut := spec.StripANSI(outBuf.String())
	rawErr := spec.StripANSI(errBuf.String())

	if opts.LogDir != "" {
		res.LogPath = archive(opts.LogDir, argv, rawOut, rawErr)
	}

	var elidedOut, elidedErr bool
	res.Stdout, elidedOut = elide(rawOut, opts.MaxLines)
	res.Stderr, elidedErr = elide(rawErr, opts.MaxLines)
	res.Elided = elidedOut || elidedErr
	return res, nil
}

// elide keeps the head and tail of long output. The head carries the schema
// (column headers, the first records); the tail carries the errors and totals.
// The middle is almost never what you needed.
func elide(s string, maxLines int) (string, bool) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "", false
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s, false
	}
	head := maxLines * 2 / 3
	tail := maxLines - head
	out := append([]string{}, lines[:head]...)
	out = append(out, fmt.Sprintf("… [%d lines elided] …", len(lines)-head-tail))
	out = append(out, lines[len(lines)-tail:]...)
	return strings.Join(out, "\n"), true
}

// archive writes the full output to disk so it stays available for grepping
// after the elided version has been handed to the model.
func archive(dir string, argv []string, stdout, stderr string) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	name := fmt.Sprintf("%s-%s.log", time.Now().Format("20060102-150405.000"), sanitize(argv))
	p := filepath.Join(dir, name)
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n\n--- stdout ---\n%s\n--- stderr ---\n%s\n", Quote(argv), stdout, stderr)
	if os.WriteFile(p, []byte(b.String()), 0o644) != nil {
		return ""
	}
	return p
}

func sanitize(argv []string) string {
	s := strings.Join(argv, "-")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// Quote renders an argv for display. It is for humans reading the approval
// prompt — nothing is ever executed through a shell.
func Quote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$`*?[]{}()|&;<>#~!") {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}
