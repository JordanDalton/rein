package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Feed generated configuration as explicit invocation overrides so this test
// does not require writing trust records or loading the user's config/hooks.
func runtimeTOML(value any) string {
	switch v := value.(type) {
	case map[string]any:
		var keys, parts []string
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%q=%s", key, runtimeTOML(v[key])))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		var parts []string
		for _, item := range v {
			parts = append(parts, runtimeTOML(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
}

func TestCodexRuntimeGuard(t *testing.T) {
	if os.Getenv("REIN_RUNTIME_TESTS") != "1" {
		t.Skip("opt in with REIN_RUNTIME_TESTS=1; requires installed Codex CLI")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root := configureTestDir(t)
	initRepo := exec.Command("git", "init", "--quiet", root)
	if output, err := initRepo.CombinedOutput(); err != nil {
		t.Fatalf("fixture git init: %v %s", err, output)
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "rein-fixture")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(helper, "'", "'\\''") + "' -test.run=^TestHarnessRuntimeHelper$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config, err := mergePersistent(nil, ".codex/config.toml", harnessProfile{Host: "codex", Rein: wrapper})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".codex/config.toml"), config, 0600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(root, "mcp-calls")
	marker := filepath.Join(root, "forbidden-marker")
	catalog := filepath.Join(root, "models.json")
	model := `{"models":[{"slug":"rein-fixture","display_name":"Rein fixture","description":"Local scripted model, no inference","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"medium","description":"fixture"}],"shell_type":"unified_exec","visibility":"list","supported_in_api":true,"priority":1,"base_instructions":"Run only the deterministic fixture calls.","supports_reasoning_summaries":false,"support_verbosity":false,"default_verbosity":null,"apply_patch_tool_type":"freeform","truncation_policy":{"mode":"tokens","limit":10000},"context_window":128000,"effective_context_window_percent":95,"experimental_supported_tools":[],"input_modalities":["text"],"supports_search_tool":true}]}`
	if err := os.WriteFile(catalog, []byte(model), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := 0
	var inputs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.Error(w, "not found", 404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		requests++
		inputs = append(inputs, string(body))
		items := []any{map[string]any{"id": "msg_done", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "fixture complete", "annotations": []any{}}}}}
		if requests == 1 {
			items = []any{map[string]any{"type": "tool_search_call", "id": "search_1", "call_id": "search_1", "execution": "client", "status": "completed", "arguments": map[string]any{"query": "rein", "limit": 4}}}
		}
		if requests == 2 {
			items = []any{}
			targets := []struct {
				name string
				args any
			}{
				{"exec_command", map[string]any{"cmd": "touch " + marker}},
				{"apply_patch", "*** Begin Patch\n*** Add File: " + marker + "\n+forbidden\n*** End Patch"},
				{"mcp__rein__forbidden", map[string]any{}},
				{"mcp__rein__rein_list", map[string]any{}},
				{"mcp__rein__rein_in", map[string]any{}},
				{"mcp__rein__rein_spec", map[string]any{}},
			}
			for i, target := range targets {
				args, _ := json.Marshal(target.args)
				item := map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", i), "call_id": fmt.Sprintf("call_%d", i), "name": target.name, "arguments": string(args), "status": "completed"}
				if strings.HasPrefix(target.name, "mcp__rein__") {
					item["namespace"] = "mcp__rein"
					item["name"] = strings.TrimPrefix(target.name, "mcp__rein__")
				}
				if target.name == "apply_patch" {
					item["type"] = "custom_tool_call"
					delete(item, "arguments")
					item["input"] = target.args
				}
				items = append(items, item)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(event string, data any) {
			encoded, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		}
		emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("resp_%d", requests), "object": "response", "status": "in_progress", "output": []any{}}})
		for i, item := range items {
			emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
			emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
		emit("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", requests), "object": "response", "status": "completed", "output": items, "usage": map[string]any{"input_tokens": 10, "output_tokens": 10, "total_tokens": 20}}})
	}))
	defer server.Close()
	provider := fmt.Sprintf(`model_providers.fixture={name="Local fixture",base_url=%q,wire_api="responses",requires_openai_auth=false,supports_websockets=false,request_max_retries=0,stream_max_retries=0}`, server.URL)
	args := []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--dangerously-bypass-hook-trust", "--sandbox", "workspace-write", "-c", fmt.Sprintf("projects.%q.trust_level=\"trusted\"", root), "-c", "model_provider=\"fixture\"", "-c", provider, "-c", "features.plugins=false", "-c", "features.shell_snapshot=false", "-c", "model=\"gpt-5.4\"", "--json", "Run the deterministic tool routing fixture."}
	args = append(args, "-c", "model=\"rein-fixture\"", "-c", fmt.Sprintf("model_catalog_json=%q", catalog))
	var generated map[string]any
	if err := toml.Unmarshal(config, &generated); err != nil {
		t.Fatal(err)
	}
	generated["mcp_servers"].(map[string]any)["rein"].(map[string]any)["env"] = map[string]any{"REIN_RUNTIME_HELPER": "1", "REIN_RUNTIME_CALLS": calls}
	// Fixture permission consent, not a production default. Both tools are
	// pre-approved so the negative control must be stopped by Rein's hook.
	consent := map[string]any{}
	for _, name := range []string{"rein_list", "rein_in", "rein_spec", "forbidden"} {
		consent[name] = map[string]any{"approval_mode": "approve"}
	}
	generated["mcp_servers"].(map[string]any)["rein"].(map[string]any)["tools"] = consent
	for key, value := range generated {
		args = append(args, "-c", key+"="+runtimeTOML(value))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir(), "REIN_RUNTIME_HELPER=1", "REIN_RUNTIME_CALLS=" + calls}
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
		t.Fatalf("native write escaped guard\n%s", output)
	}
	called, _ := os.ReadFile(calls)
	if !runtimeExpectedCalls(called) {
		t.Fatalf("unexpected MCP dispatch: %q\n%s", called, output)
	}
	if !strings.Contains(strings.Join(inputs, "\n"), "This project routes operations through Rein MCP") {
		t.Fatalf("no observed hook denial: %s", output)
	}
	if !strings.Contains(strings.Join(inputs, "\n"), "fixture response: rein_list") {
		t.Fatal("Rein response did not reach model")
	}
	for _, expected := range []string{"unsupported call: exec_command", "Command blocked by PreToolUse hook", "Tool: mcp__rein__forbidden"} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("missing runtime evidence %q\n%s", expected, output)
		}
	}
	t.Logf("Codex: shell unavailable, patch and unapproved MCP blocked, Rein dispatched; %d requests", requests)
}
