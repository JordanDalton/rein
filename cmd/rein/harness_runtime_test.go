package main

// Opt-in tests run real installed harnesses against a scripted loopback model.
// They do not use model accounts or the production Rein control plane.
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jordandalton/rein/internal/mcp"
)

func TestHarnessRuntimeHelper(t *testing.T) {
	if os.Getenv("REIN_RUNTIME_HELPER") != "1" {
		return
	}
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "--tool-guard" {
		if err := runToolGuard(os.Stdin, os.Stdout); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	server := mcp.New("rein", "fixture", os.Stdin, os.Stdout)
	for _, name := range []string{"rein_in", "rein_list", "rein_spec", "forbidden"} {
		if name == "forbidden" && os.Getenv("REIN_RUNTIME_STRICT") == "1" {
			continue
		}
		server.Add(mcp.Tool{Name: name, Description: "Runtime fixture " + name, InputSchema: json.RawMessage(`{"type":"object","properties":{}}`), Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			f, err := os.OpenFile(os.Getenv("REIN_RUNTIME_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return "", err
			}
			defer f.Close()
			fmt.Fprintln(f, name)
			return "fixture response: " + name, nil
		}})
	}
	if err := server.Serve(context.Background()); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestClaudeRuntimeGuard(t *testing.T) {
	if os.Getenv("REIN_RUNTIME_TESTS") != "1" {
		t.Skip("opt in with REIN_RUNTIME_TESTS=1; requires installed Claude CLI")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"persistent", "strict-launch"} {
		t.Run(mode, func(t *testing.T) {
			root := configureTestDir(t)
			helper, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			wrapper := filepath.Join(root, "rein-fixture")
			script := "#!/bin/sh\nexec '" + strings.ReplaceAll(helper, "'", "'\\''") + "' -test.run=^TestHarnessRuntimeHelper$ -- \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			settings, err := mergePersistent(nil, ".claude/settings.json", harnessProfile{Host: "claude-code", Rein: wrapper})
			if err != nil {
				t.Fatal(err)
			}
			settingsFile := filepath.Join(root, "settings.json")
			if err := os.WriteFile(settingsFile, settings, 0600); err != nil {
				t.Fatal(err)
			}
			mcpConfig, err := mergePersistent(nil, ".mcp.json", harnessProfile{Host: "claude-code", Rein: wrapper})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			requests := 0
			var seen []string
			var outputs []string
			marker := filepath.Join(root, "forbidden-marker")
			if err := os.WriteFile(filepath.Join(root, "read-fixture"), []byte("PRIVATE_FIXTURE_SENTINEL"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(root, "mcp-calls")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/messages") {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{}`)
					return
				}
				var body struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
					Messages json.RawMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, "bad request", 400)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				requests++
				for _, tool := range body.Tools {
					seen = append(seen, tool.Name)
				}
				outputs = append(outputs, string(body.Messages))
				content := []any{map[string]any{"type": "text", "text": "fixture complete"}}
				stop := "end_turn"
				if requests == 1 {
					content = []any{}
					targets := []struct {
						name  string
						input any
					}{
						{"Bash", map[string]any{"command": "touch " + marker}},
						{"Write", map[string]any{"file_path": marker, "content": "forbidden"}},
						{"Read", map[string]any{"file_path": filepath.Join(root, "read-fixture")}},
						{"ToolSearch", map[string]any{"query": "rein"}},
						{"mcp__rein__forbidden", map[string]any{}},
						{"mcp__rein__rein_list", map[string]any{}},
						{"mcp__rein__rein_in", map[string]any{}},
						{"mcp__rein__rein_spec", map[string]any{}},
					}
					for i, target := range targets {
						content = append(content, map[string]any{"type": "tool_use", "id": fmt.Sprintf("call_%d", i), "name": target.name, "input": target.input})
					}
					stop = "tool_use"
				}
				w.Header().Set("Content-Type", "text/event-stream")
				emit := func(event string, data any) {
					encoded, _ := json.Marshal(data)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
				}
				emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 1}}})
				for i, block := range content {
					emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": block})
					emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
				}
				emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 10}})
				emit("message_stop", map[string]any{"type": "message_stop"})
			}))
			defer server.Close()
			args := []string{"-p", "Run the deterministic tool routing fixture.", "--model", "claude-sonnet-4-6", "--setting-sources", "", "--no-session-persistence", "--permission-mode", "dontAsk", "--allowedTools", "mcp__rein__*", "--output-format", "json"}
			if mode == "strict-launch" {
				args = append(args, harnessArgs(harnessProfile{Host: "claude-code", Rein: wrapper})...)
			} else {
				args = append(args, "--settings", settingsFile, "--strict-mcp-config", "--mcp-config", string(mcpConfig))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Dir = root
			// Allowlist env: never pass account tokens, production URLs, or user config overrides.
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir(), "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "claude-config"), "ANTHROPIC_API_KEY=fixture-not-a-secret", "ANTHROPIC_BASE_URL=" + server.URL, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "ENABLE_TOOL_SEARCH=true", "REIN_RUNTIME_HELPER=1", "REIN_RUNTIME_CALLS=" + calls}
			if mode == "strict-launch" {
				cmd.Env = append(cmd.Env, "REIN_RUNTIME_STRICT=1")
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("harness failed: %v\n%s", err, output)
			}
			mu.Lock()
			defer mu.Unlock()
			if requests < 2 {
				t.Fatalf("tool results not observed: %s", output)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("native write escaped guard")
			}
			called, _ := os.ReadFile(calls)
			if !runtimeExpectedCalls(called) {
				t.Fatalf("unexpected MCP dispatch: %q\n%s", called, output)
			}
			if !strings.Contains(strings.Join(outputs, "\n"), "fixture response: rein_list") {
				t.Fatal("Rein response did not reach model")
			}
			if mode == "persistent" && !strings.Contains(strings.Join(outputs, "\n"), "This project routes operations through Rein MCP") {
				t.Fatal("no observed Rein guard denial")
			}
			if strings.Contains(strings.Join(outputs, "\n"), "PRIVATE_FIXTURE_SENTINEL") {
				t.Fatal("native read escaped guard")
			}
			if !strings.Contains(strings.Join(seen, "\n"), "mcp__rein__rein_list") {
				t.Fatal("Rein was not advertised upfront")
			}
			t.Logf("%s: blocked native writes and unapproved MCP; Rein dispatched; %d model requests, %d advertised tool entries", mode, requests, len(seen))
		})
	}
}

func runtimeExpectedCalls(called []byte) bool {
	lines := strings.Fields(string(called))
	if len(lines) != 3 {
		return false
	}
	counts := map[string]int{}
	for _, line := range lines {
		counts[line]++
	}
	return counts["rein_in"] == 1 && counts["rein_list"] == 1 && counts["rein_spec"] == 1
}
