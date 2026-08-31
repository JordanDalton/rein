package planner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Agent CLIs — `claude`, `codex`, `grok` — are the zero-config backends: they
// reuse credentials the user has already set up, so rein works with no
// API key anywhere.
//
// Two things matter when driving one as a planner. They are agents with tool
// access of their own, so each is pinned to the most restrictive mode it
// offers: the planner's job is to emit JSON, not to go exploring the
// filesystem. And their structured-output envelopes differ and change between
// releases, so each is asked for plain text and the reply goes through
// ParsePlan's tolerant extractor instead.

// runAgentCLI executes a CLI planner and returns its stdout.
func runAgentCLI(ctx context.Context, bin string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(nil)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errBuf.String())
		if detail == "" {
			detail = strings.TrimSpace(out.String())
		}
		if _, lookErr := exec.LookPath(bin); lookErr != nil {
			return "", fmt.Errorf("%s is not installed or not on PATH", bin)
		}
		return "", fmt.Errorf("%s failed: %w: %s", bin, err, clip(detail, 400))
	}
	return out.String(), nil
}

// CodexCLI drives OpenAI's Codex CLI in its non-interactive `exec` mode.
type CodexCLI struct {
	Bin   string // default "codex"
	Model string
}

func (c *CodexCLI) Name() string {
	if c.Model != "" {
		return "codex-cli:" + c.Model
	}
	return "codex-cli"
}

func (c *CodexCLI) Complete(ctx context.Context, system, user string) (string, error) {
	bin := c.Bin
	if bin == "" {
		bin = "codex"
	}

	// `codex exec` writes its final message to this file, which sidesteps the
	// event log it otherwise streams to stdout.
	f, err := os.CreateTemp("", "rein-codex-*.txt")
	if err != nil {
		return "", err
	}
	last := f.Name()
	f.Close()
	defer os.Remove(last)

	args := []string{
		"exec",
		"--skip-git-repo-check",  // the wrapped tool need not be a repo
		"--sandbox", "read-only", // the planner plans; it does not act
		"--output-last-message", last,
	}
	if c.Model != "" {
		args = append(args, "-m", c.Model)
	}
	args = append(args, system+"\n\n"+user)

	stdout, err := runAgentCLI(ctx, bin, args)
	if err != nil {
		return "", err
	}
	body, readErr := os.ReadFile(last)
	if readErr != nil || len(bytes.TrimSpace(body)) == 0 {
		// Fall back to stdout: a future release may stop writing the file.
		return stdout, nil
	}
	return string(body), nil
}

// GrokCLI drives xAI's Grok CLI in headless mode.
type GrokCLI struct {
	Bin   string // default "grok"
	Model string
}

func (g *GrokCLI) Name() string {
	if g.Model != "" {
		return "grok-cli:" + g.Model
	}
	return "grok-cli"
}

func (g *GrokCLI) Complete(ctx context.Context, system, user string) (string, error) {
	bin := g.Bin
	if bin == "" {
		bin = "grok"
	}
	args := []string{
		"-p", system + "\n\n" + user,
		"--output-format", "plain",
		"--max-turns", "1", // one completion, not an agent loop
		"--disable-web-search",
	}
	if g.Model != "" {
		args = append(args, "-m", g.Model)
	}
	return runAgentCLI(ctx, bin, args)
}
