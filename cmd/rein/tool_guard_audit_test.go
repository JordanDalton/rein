package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestToolGuardAuditsBlockedOperation(t *testing.T) {
	var output bytes.Buffer
	calls := 0
	err := runToolGuardWithAudit(strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"rm -rf test.sqlite","content":"private file contents"}}`), &output, func(ctx context.Context, operation map[string]any) error {
		calls++
		if operation["tool"] != "Bash" || operation["command"] != "rm -rf test.sqlite" || operation["content"] != nil {
			t.Errorf("unexpected summary: %#v", operation)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing audit deadline")
		}
		return nil
	})
	if err != nil || calls != 1 || !strings.Contains(output.String(), `"permissionDecision":"deny"`) {
		t.Fatalf("guard result: %v, %d, %s", err, calls, output.String())
	}
}

func TestToolGuardAuditFailureCannotAllowTool(t *testing.T) {
	for _, audit := range []func(context.Context, map[string]any) error{
		func(context.Context, map[string]any) error { return errors.New("secret connection details") },
		func(ctx context.Context, _ map[string]any) error { <-ctx.Done(); return ctx.Err() },
	} {
		var output bytes.Buffer
		start := time.Now()
		err := runToolGuardWithAudit(strings.NewReader("{}"), &output, audit)
		if err != nil || time.Since(start) > 3*time.Second {
			t.Fatalf("guard did not return promptly: %v", err)
		}
		var result map[string]any
		if json.Unmarshal(output.Bytes(), &result) != nil || !strings.Contains(output.String(), `"permissionDecision":"deny"`) || !strings.Contains(output.String(), "could not be recorded") || strings.Contains(output.String(), "secret connection") {
			t.Fatalf("unsafe failure output: %s", output.String())
		}
	}
}

func TestToolGuardDoesNotAuditAllowedTools(t *testing.T) {
	var output bytes.Buffer
	err := runToolGuardWithAudit(strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"mcp__rein__rein_in"}`), &output, func(context.Context, map[string]any) error {
		t.Error("allowed tool audited as blocked")
		return nil
	})
	if err != nil || output.String() != "{}\n" {
		t.Fatalf("%v: %s", err, output.String())
	}
}

func TestToolGuardSummaryRedactsAndBoundsInput(t *testing.T) {
	operation := toolGuardOperation("Bash", map[string]any{
		"command": "TOKEN=super-secret-value",
		"path":    strings.Repeat("a", 10000),
		"content": "private contents",
	})
	if strings.Contains(operation["command"].(string), "super-secret-value") || len(operation["path"].(string)) > 4100 || operation["content"] != nil {
		t.Fatalf("unsafe summary: %#v", operation)
	}
}

func TestToolGuardPostsBlockedAudit(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("device credentials require macOS")
	}
	t.Setenv("REIN_HOME", t.TempDir())
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nprintf guard-test-token\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/rein/audit-events" || r.Header.Get("Authorization") != "Bearer guard-test-token" {
			t.Errorf("unexpected audit request: %s %s", r.Method, r.URL.Path)
		}
		var payload struct {
			Event     string
			Caller    string
			Operation map[string]any
			Metadata  map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Event != "blocked" || payload.Caller != "tool-guard" || payload.Operation["tool"] != "ToolSearch" || payload.Metadata["executed"] != false || payload.Metadata["stage"] != "tool_guard" || payload.Metadata["reason"] != toolGuardReason {
			t.Errorf("unexpected event: %#v", payload)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	if err := saveCloudProfile(cloudProfile{ControlURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runToolGuard(strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"ToolSearch","tool_input":{"query":"rein"}}`), &output); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || strings.Contains(output.String(), "could not be recorded") {
		t.Fatalf("audit missing: %d %s", calls, output.String())
	}
}
