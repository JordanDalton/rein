package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestToolGuardDefaultDeny(t *testing.T) {
	for _, name := range []string{"Bash", "Read", "Write", "Edit", "apply_patch", "exec_command", "write_stdin", "python", "WebSearch", "Agent", "spawn_agent", "mcp__other__run", "mcp__rein_fake__rein_in", "mcp__rein__unknown", ""} {
		input, _ := json.Marshal(map[string]string{"hook_event_name": "PreToolUse", "tool_name": name})
		var output bytes.Buffer
		if err := runToolGuard(bytes.NewReader(input), &output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), `"permissionDecision":"deny"`) {
			t.Fatalf("not denied: %s: %s", name, output.String())
		}
	}
	for _, name := range []string{"rein_in", "rein_list", "rein_spec"} {
		var output bytes.Buffer
		input := `{"hook_event_name":"PreToolUse","tool_name":"mcp__rein__` + name + `"}`
		if err := runToolGuard(strings.NewReader(input), &output); err != nil {
			t.Fatal(err)
		}
		if output.String() != "{}\n" {
			t.Fatalf("must defer Rein tool: %s", output.String())
		}
	}
	for _, input := range []string{"", "null", "{}", "not json", `{"tool_name":"mcp__rein__rein_in"}`, `{"hook_event_name":"PreToolUse","tool_name":"mcp__rein__rein_in"}{}`, strings.Repeat("x", 1024*1024+1)} {
		var output bytes.Buffer
		if err := runToolGuard(strings.NewReader(input), &output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), `"permissionDecision":"deny"`) {
			t.Fatal("malformed input passed")
		}
	}
}

func TestClaudePersistentReinLoadsWithoutToolSearch(t *testing.T) {
	data, err := mergePersistent(nil, ".mcp.json", harnessProfile{Host: "claude-code", Rein: "/usr/local/bin/rein"})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Servers map[string]struct {
			AlwaysLoad bool `json:"alwaysLoad"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if !document.Servers["rein"].AlwaysLoad {
		t.Fatal("Rein cannot depend on the blocked ToolSearch tool")
	}
}

func TestPersistentMergePreservesSettings(t *testing.T) {
	p := harnessProfile{Host: "claude-code", Rein: "/tmp/Jordan's Rein/rein", Backend: "ollama", Model: "local"}
	original := []byte(`{"model":"outer-model","permissions":{"allow":["Read(foo)"],"deny":["Read(secret)"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`)
	result, err := mergePersistent(original, ".claude/settings.json", p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "outer-model" || !strings.Contains(string(result), "Read(secret)") || !strings.Contains(string(result), "Read(foo)") || !strings.Contains(string(result), "Stop") {
		t.Fatal("lost existing settings")
	}
	if !strings.Contains(string(result), "|| exit 2") {
		t.Fatal("missing failure fallback")
	}
	p.Host = "codex"
	result, err = mergePersistent([]byte("model = 'outer'\n[features]\napps = false\n[mcp_servers.other]\ncommand = 'existing'\n"), ".codex/config.toml", p)
	if err != nil {
		t.Fatal(err)
	}
	doc = map[string]any{}
	if err := toml.Unmarshal(result, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "outer" || doc["web_search"] != "disabled" {
		t.Fatal("incorrect top-level settings")
	}
	if !strings.Contains(string(result), "existing") || !strings.Contains(string(result), "PreToolUse") || !strings.Contains(string(result), "ollama") {
		t.Fatal("incomplete TOML merge")
	}
	// Existing TOML hook arrays must survive too.
	if _, err := mergePersistent(result, ".codex/config.toml", p); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentRejectsMalformedSettings(t *testing.T) {
	for _, input := range []string{"null", "{", `{"hooks":[]}`, `{"hooks":{"PreToolUse":{}}}`, `{"disableAllHooks":true}`, `{"permissions":{"deny":"Bash"}}`} {
		if _, err := mergePersistent([]byte(input), ".claude/settings.json", harnessProfile{Host: "claude-code", Rein: "/bin/rein"}); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestPersistentPreviewApplyDriftUndo(t *testing.T) {
	for _, host := range []string{"claude-code", "codex"} {
		t.Run(host, func(t *testing.T) {
			dir := configureTestDir(t)
			t.Chdir(dir)
			if err := configurePersistent(host, "", "", false, false, false, false); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(".rein"); !os.IsNotExist(err) {
				t.Fatal("preview wrote files")
			}
			paths := persistentPaths(host)
			first := paths[0]
			if err := os.MkdirAll(filepath.Dir(first), 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte("{\n  \"model\": \"keep\"\n}\n")
			if host == "codex" {
				original = []byte("# keep comment\nmodel = 'keep'\n")
			}
			if err := os.WriteFile(first, original, 0640); err != nil {
				t.Fatal(err)
			}
			if err := configurePersistent(host, "", "", false, true, false, false); err != nil {
				t.Fatal(err)
			}
			installed, _ := os.ReadFile(first)
			if err := configurePersistent(host, "", "", false, true, false, false); err != nil {
				t.Fatal("apply not idempotent:", err)
			}
			if err := os.WriteFile(first, append(installed, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			if err := configurePersistent(host, "", "", false, false, true, false); err == nil {
				t.Fatal("drift not detected")
			}
			if err := configurePersistent(host, "", "", false, false, false, true); err == nil {
				t.Fatal("undo clobbers edits")
			}
			if err := os.WriteFile(first, installed, 0600); err != nil {
				t.Fatal(err)
			}
			if err := configurePersistent(host, "", "", false, false, false, true); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(first)
			if !bytes.Equal(restored, original) {
				t.Fatal("not exact restore")
			}
			info, _ := os.Stat(first)
			if info.Mode().Perm() != 0640 {
				t.Fatal("mode not restored")
			}
			if host == "claude-code" {
				if _, err := os.Stat(".mcp.json"); !os.IsNotExist(err) {
					t.Fatal("new MCP file not removed")
				}
			}
		})
	}
}

func TestPersistentGatewaySetupIsIdempotent(t *testing.T) {
	dir := configureTestDir(t)
	t.Chdir(dir)

	if err := configurePersistent("codex", "", "", true, true, false, false); err != nil {
		t.Fatal(err)
	}
	if err := configurePersistent("codex", "", "", true, false, false, false); err != nil {
		t.Fatal("matching gateway preview is not idempotent:", err)
	}
	if err := configurePersistent("codex", "", "", true, true, false, false); err != nil {
		t.Fatal("matching gateway apply is not idempotent:", err)
	}
	if err := configurePersistent("codex", "", "", false, false, false, false); err == nil {
		t.Fatal("transport change did not require undo")
	}
}

func TestPersistentRefusesSymlinksAndForeignReceipts(t *testing.T) {
	dir := configureTestDir(t)
	t.Chdir(dir)
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ".claude"); err != nil {
		t.Fatal(err)
	}
	if err := configurePersistent("claude-code", "", "", false, true, false, false); err == nil {
		t.Fatal("followed symlink")
	}
	if err := validatePersistentReceipt(persistentReceipt{Version: 1, Host: "codex", Binary: "/bin/rein", Files: []persistentFile{{Path: "../../elsewhere", After: []byte("x")}}}, "codex"); err == nil {
		t.Fatal("accepted foreign path")
	}
}
