package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configureTestDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigureStrictClaudeArguments(t *testing.T) {
	p := harnessProfile{Version: 1, Host: "claude-code", Rein: "/tmp/Rein with spaces/rein"}
	args := harnessArgs(p)
	if len(args) != 5 || args[0] != "--tools" || args[1] != "" || args[2] != "--strict-mcp-config" {
		t.Fatalf("unsafe arguments: %#v", args)
	}
	var config struct {
		Servers map[string]struct {
			Command    string   `json:"command"`
			Args       []string `json:"args"`
			AlwaysLoad bool     `json:"alwaysLoad"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[4]), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Servers) != 1 || config.Servers["rein"].Command != p.Rein {
		t.Fatalf("wrong server config: %s", args[4])
	}
	if !config.Servers["rein"].AlwaysLoad {
		t.Fatal("Rein tools must load without native ToolSearch")
	}
	if strings.Join(config.Servers["rein"].Args, " ") != "mcp --agent claude-code" {
		t.Fatal("unexpected permissions")
	}
}

func TestConfigureCanUsePersistentGateway(t *testing.T) {
	for _, host := range []string{"claude-code", "codex"} {
		args := harnessArgs(harnessProfile{Host: host, Rein: "/bin/rein", Gateway: true})
		encoded, _ := json.Marshal(args)
		if !strings.Contains(string(encoded), `gateway`) || !strings.Contains(string(encoded), `connect`) || !strings.Contains(string(encoded), host) {
			t.Fatalf("%s does not use gateway bridge: %s", host, encoded)
		}
		if strings.Contains(string(encoded), `\"mcp\"`) {
			t.Fatalf("%s still launches rein mcp: %s", host, encoded)
		}
	}
}

func TestConfigurePlannerOptionsOnlyReachMCP(t *testing.T) {
	for _, host := range []string{"claude-code", "codex"} {
		p := harnessProfile{Version: 1, Host: host, Rein: "/bin/rein", Backend: "ollama", Model: `local-model "quoted"`}
		args := harnessArgs(p)
		for _, arg := range args {
			if arg == "--model" || arg == "--backend" {
				t.Fatal("planner flag leaked to outer harness")
			}
		}
		var mcpArgs []string
		if host == "claude-code" {
			var config struct {
				Servers map[string]struct {
					Args []string `json:"args"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal([]byte(args[4]), &config); err != nil {
				t.Fatal(err)
			}
			mcpArgs = config.Servers["rein"].Args
		} else {
			start := strings.Index(args[3], "args=") + len("args=")
			end := strings.LastIndex(args[3], ",enabled=")
			if err := json.Unmarshal([]byte(args[3][start:end]), &mcpArgs); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{"mcp", "--agent", host, "--backend", "ollama", "--model", p.Model}
		if strings.Join(mcpArgs, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("wrong MCP args: %#v", mcpArgs)
		}
	}
}

func TestConfigurePersistsPlannerOptions(t *testing.T) {
	t.Chdir(configureTestDir(t))
	if err := cmdConfigure(context.Background(), []string{"codex", "--backend", "ollama", "--model", "local-model", "--apply"}); err != nil {
		t.Fatal(err)
	}
	p, err := readHarnessProfile(filepath.Join(".rein", "harnesses", "codex.json"), "codex")
	if err != nil || p.Backend != "ollama" || p.Model != "local-model" {
		t.Fatalf("planner options not saved: %#v %v", p, err)
	}
	if err := cmdConfigure(context.Background(), []string{"codex", "--launch", "--model", "ignored"}); err == nil {
		t.Fatal("must not silently ignore model override")
	}
}

func TestConfigureCodexPartialCoverage(t *testing.T) {
	args := harnessArgs(harnessProfile{Host: "codex", Rein: `C:\Program Files\Rein\rein.exe`})
	if args[1] != "features.shell_tool=false" || !strings.Contains(args[3], "required=true") || !strings.Contains(args[3], "enabled=true") {
		t.Fatalf("wrong args: %#v", args)
	}
	if !strings.Contains(harnessCoverage("codex"), "PARTIAL") {
		t.Fatal("must disclose partial coverage")
	}
	if strings.Contains(strings.Join(args, " "), "danger-full-access") {
		t.Fatal("must preserve sandbox")
	}
}

func TestConfigureSaveIdempotentAndUndo(t *testing.T) {
	path := filepath.Join(configureTestDir(t), "harnesses", "claude-code.json")
	data := []byte("new profile")
	if err := saveHarnessProfile(path, data); err != nil {
		t.Fatal(err)
	}
	if err := saveHarnessProfile(path, data); err != nil {
		t.Fatal(err)
	}
	if err := undoHarnessProfile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("new profile should be removed")
	}
}

func TestConfigurePreservesPreviousBytes(t *testing.T) {
	path := filepath.Join(configureTestDir(t), "codex.json")
	before := []byte("original configuration\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveHarnessProfile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := undoHarnessProfile(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("original was not restored: %s, %v", after, err)
	}
}

func TestConfigureUndoRefusesEdits(t *testing.T) {
	path := filepath.Join(configureTestDir(t), "codex.json")
	if err := saveHarnessProfile(path, []byte("installed")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := undoHarnessProfile(path); err == nil {
		t.Fatal("must reject changed file")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "user edit" {
		t.Fatal("clobbered user edit")
	}
}

func TestConfigureRefusesSymlinks(t *testing.T) {
	dir := configureTestDir(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if err := saveHarnessProfile(link, []byte("new")); err == nil {
		t.Fatal("must refuse symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "untouched" {
		t.Fatal("changed symlink target")
	}
}

func TestConfigurePreviewDoesNotWrite(t *testing.T) {
	dir := configureTestDir(t)
	t.Chdir(dir)
	if err := cmdConfigure(context.Background(), []string{"claude-code", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("dry run wrote files")
	}
}

func TestConfigureRejectsConflictingActions(t *testing.T) {
	for _, args := range [][]string{{"codex", "--apply", "--dry-run"}, {"codex", "--launch", "--undo"}, {"codex", "--register"}, {"codex", "--scope", "invalid"}, {"codex", "--launch", "--dangerously-bypass-approvals-and-sandbox"}} {
		if err := cmdConfigure(context.Background(), args); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
}

func TestConfigureApplyAndUndoCommand(t *testing.T) {
	dir := configureTestDir(t)
	t.Chdir(dir)
	ctx := context.Background()
	if err := cmdConfigure(ctx, []string{"codex", "--apply"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".rein", "harnesses", "codex.json")
	p, err := readHarnessProfile(path, "codex")
	if err != nil || p.Host != "codex" || !filepath.IsAbs(p.Rein) {
		t.Fatalf("invalid saved profile: %#v %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".codex")); !os.IsNotExist(err) {
		t.Fatal("must not write native harness configuration")
	}
	if err := cmdConfigure(ctx, []string{"codex", "--undo"}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureRejectsInvalidProfiles(t *testing.T) {
	path := filepath.Join(configureTestDir(t), "codex.json")
	for _, data := range []string{`{"version":2,"host":"codex","rein_binary":"/bin/rein"}`, `{"version":1,"host":"claude-code","rein_binary":"/bin/rein"}`, `{"version":1,"host":"codex","rein_binary":"relative/rein"}`, `not json`} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readHarnessProfile(path, "codex"); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestConfigureKeepsUndoPointWhenReplacingDifferentProfile(t *testing.T) {
	path := filepath.Join(configureTestDir(t), "codex.json")
	if err := saveHarnessProfile(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := saveHarnessProfile(path, []byte("second")); err == nil {
		t.Fatal("must retain original undo point")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "first" {
		t.Fatal("changed installed profile")
	}
}
