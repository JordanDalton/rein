package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
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
