package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestSetupMCPHelper(t *testing.T) {
	if os.Getenv("REIN_SETUP_HELPER") != "1" {
		return
	}
	var request map[string]any
	_ = json.NewDecoder(os.Stdin).Decode(&request)
	if os.Getenv("REIN_SETUP_BAD_RESPONSE") == "1" {
		_, _ = io.WriteString(os.Stdout, "{\"id\":1,\"error\":{\"message\":\"not ready\"}}\n")
	} else {
		_, _ = io.WriteString(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2025-11-25\"}}\n")
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestCheckClaudeMCP(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("REIN_SETUP_HELPER", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"rein": map[string]any{"command": binary, "args": []string{"-test.run=^TestSetupMCPHelper$"}}}})
	if err := os.WriteFile(".mcp.json", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkClaudeMCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REIN_SETUP_BAD_RESPONSE", "1")
	if err := checkClaudeMCP(context.Background()); err == nil {
		t.Fatal("accepted failed handshake")
	}
}

func TestCheckCodexMCP(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("REIN_SETUP_HELPER", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := toml.Marshal(map[string]any{"mcp_servers": map[string]any{"rein": map[string]any{"command": binary, "args": []string{"-test.run=^TestSetupMCPHelper$"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(".codex", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".codex/config.toml", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkHarnessMCP(context.Background(), "codex"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REIN_SETUP_BAD_RESPONSE", "1")
	if err := checkHarnessMCP(context.Background(), "codex"); err == nil {
		t.Fatal("accepted failed handshake")
	}
}

func TestConfirmSetup(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"y\n", true}, {"YES\n", true}, {"\n", false}, {"n\n", false}, {"", false}, {"yes", false},
	} {
		if got := confirmSetup(strings.NewReader(tc.input), io.Discard); got != tc.want {
			t.Errorf("confirm %q = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestGuidedSetupOutputIsConcise(t *testing.T) {
	var output strings.Builder
	printGuidedSetupPlan(&output, "codex", "/tmp/project")
	printGuidedSetupComplete(&output, "codex")
	got := output.String()
	for _, want := range []string{
		"Rein setup — Codex",
		"Connection  Persistent Rein Gateway",
		"No model requests or tool commands will run",
		"Ready — Codex is connected through Rein Gateway.",
		"✓ MCP handshake verified",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in guided output:\n%s", want, got)
		}
	}
	for _, noisy := range []string{"Exact argument array", "PARTIAL COVERAGE:", `"gateway": true`} {
		if strings.Contains(got, noisy) {
			t.Errorf("guided output contains internal detail %q:\n%s", noisy, got)
		}
	}
}

func TestGuidedHarnessProfileIsIdempotent(t *testing.T) {
	t.Chdir(configureTestDir(t))
	if err := guidedHarnessProfile("codex", false); err != nil {
		t.Fatal(err)
	}
	if err := guidedHarnessProfile("codex", true); err != nil {
		t.Fatal(err)
	}
	if err := guidedHarnessProfile("codex", true); err != nil {
		t.Fatal("matching profile is not idempotent:", err)
	}
	profile, err := readHarnessProfile(filepath.Join(".rein", "harnesses", "codex.json"), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Gateway {
		t.Fatal("guided profile does not use the gateway")
	}
}
