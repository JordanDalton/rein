package planner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// DefaultCLIModel is the model the claude CLI backend uses when none is
// configured. The planner's job — pick the next argv from help text and emit
// a small JSON object — is comfortably within Haiku's ability, and it answers
// in a fraction of the time of the CLI's default Opus-class model.
const DefaultCLIModel = "haiku"

// ClaudeCLI drives the local `claude` binary. It is the zero-config backend:
// it reuses whatever credentials the user's Claude Code install already has,
// so rein works with no API key set.
//
// The process is started once per run in streaming mode and kept alive: the
// CLI's ~2s Node startup is paid on the first step only, and because the
// session retains earlier turns, later steps send just the newest command
// result instead of the whole transcript.
type ClaudeCLI struct {
	Bin   string // default "claude"
	Model string // default DefaultCLIModel

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	errBuf *bytes.Buffer
}

func (c *ClaudeCLI) model() string {
	if c.Model != "" {
		return c.Model
	}
	return DefaultCLIModel
}

func (c *ClaudeCLI) Name() string { return "claude-cli:" + c.model() }

// streamEvent is the subset of `--output-format stream-json` we care about.
type streamEvent struct {
	Type    string `json:"type"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
	Subtype string `json:"subtype"`
}

// userMessage is one `--input-format stream-json` input line.
type userMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// Complete satisfies Backend for one-shot use; it is a session of one turn.
func (c *ClaudeCLI) Complete(ctx context.Context, system, user string) (string, error) {
	return c.Send(ctx, system, user)
}

// Send delivers one message into the live session, starting the process on
// first use. The system prompt is fixed at process start; rein sends the same
// one on every call, so later values are simply the same value.
func (c *ClaudeCLI) Send(ctx context.Context, system, message string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.start(system); err != nil {
		return "", err
	}

	var m userMessage
	m.Type = "user"
	m.Message.Role = "user"
	m.Message.Content = append(m.Message.Content, struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: message})
	line, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return "", c.fail(fmt.Errorf("writing to %s: %w", c.bin(), err))
	}

	// Read events off the session until this turn's result arrives. The read
	// happens in a goroutine so a cancelled ctx can still interrupt it.
	type outcome struct {
		text string
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		for {
			raw, err := c.out.ReadString('\n')
			if err != nil {
				ch <- outcome{err: fmt.Errorf("%s session ended: %w: %s",
					c.bin(), err, clip(c.errBuf.String(), 400))}
				return
			}
			var ev streamEvent
			if json.Unmarshal([]byte(raw), &ev) != nil || ev.Type != "result" {
				continue // progress events, partial messages, verbose noise
			}
			if ev.IsError {
				ch <- outcome{err: fmt.Errorf("%s returned an error (%s): %s",
					c.bin(), ev.Subtype, clip(ev.Result, 300))}
				return
			}
			ch <- outcome{text: ev.Result}
			return
		}
	}()

	select {
	case o := <-ch:
		if o.err != nil {
			return "", c.fail(o.err)
		}
		return o.text, nil
	case <-ctx.Done():
		c.stop()
		return "", ctx.Err()
	}
}

// Close terminates the session process, if any.
func (c *ClaudeCLI) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stop()
	return nil
}

func (c *ClaudeCLI) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "claude"
}

// start launches the CLI in streaming mode. Callers hold c.mu.
func (c *ClaudeCLI) start(system string) error {
	if c.cmd != nil {
		return nil
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", // stream-json output requires it in print mode
		"--system-prompt", system,
		// The planner needs a bare completion; skipping the user's configured
		// MCP servers avoids spawning every server at startup.
		"--strict-mcp-config",
		"--model", c.model(),
	}

	cmd := exec.Command(c.bin(), args...)
	// The plan is a ~100-token JSON object; extended thinking multiplies the
	// latency of every step several times over without changing the argv.
	cmd.Env = append(os.Environ(), "MAX_THINKING_TOKENS=0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errBuf := &bytes.Buffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		if _, lookErr := exec.LookPath(c.bin()); lookErr != nil {
			return fmt.Errorf("%s is not installed or not on PATH", c.bin())
		}
		return fmt.Errorf("starting %s: %w", c.bin(), err)
	}

	c.cmd = cmd
	c.stdin = stdin
	// Result lines carry full usage metadata and can run long.
	c.out = bufio.NewReaderSize(stdout, 1<<20)
	c.errBuf = errBuf
	return nil
}

// fail tears the session down so the next Send starts fresh, and passes the
// error through. Callers hold c.mu.
func (c *ClaudeCLI) fail(err error) error {
	c.stop()
	return err
}

// stop kills the session process. Callers hold c.mu.
func (c *ClaudeCLI) stop() {
	if c.cmd == nil {
		return
	}
	c.stdin.Close() // the CLI exits when stdin closes
	c.cmd.Process.Kill()
	c.cmd.Wait()
	c.cmd = nil
	c.stdin = nil
	c.out = nil
	c.errBuf = nil
}

// interface checks
var (
	_ Sessional = (*ClaudeCLI)(nil)
	_ io.Closer = (*ClaudeCLI)(nil)
)
