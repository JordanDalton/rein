package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeCLI drives the local `claude` binary in print mode. It is the zero-config
// backend: it reuses whatever credentials the user's Claude Code install already
// has, so rein works with no API key set.
type ClaudeCLI struct {
	Bin   string // default "claude"
	Model string // optional; empty uses the CLI's configured default
}

func (c *ClaudeCLI) Name() string {
	if c.Model != "" {
		return "claude-cli:" + c.Model
	}
	return "claude-cli"
}

// cliResult is the subset of `--output-format json` we care about.
type cliResult struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
	Subtype string `json:"subtype"`
}

func (c *ClaudeCLI) Complete(ctx context.Context, system, user string) (string, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	args := []string{"-p", user, "--system-prompt", system, "--output-format", "json"}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(nil)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s failed: %w: %s", bin, err, strings.TrimSpace(errBuf.String()))
	}

	var r cliResult
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &r); err != nil {
		return "", fmt.Errorf("could not parse %s JSON output: %w", bin, err)
	}
	if r.IsError {
		return "", fmt.Errorf("%s returned an error (%s): %s", bin, r.Subtype, clip(r.Result, 300))
	}
	return r.Result, nil
}
