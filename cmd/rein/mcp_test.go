package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordandalton/rein/internal/loop"
	"github.com/jordandalton/rein/internal/planner"
	"github.com/jordandalton/rein/internal/spec"
)

type scriptedBackend struct {
	replies []string
	calls   int
}

func (f *scriptedBackend) Name() string { return "scripted" }

func (f *scriptedBackend) Complete(context.Context, string, string) (string, error) {
	if f.calls >= len(f.replies) {
		return `{"action":"answer","answer":"out of script"}`, nil
	}
	r := f.replies[f.calls]
	f.calls++
	return r, nil
}

func mcpDepsFor(t *testing.T, ceiling loop.Approval, replies ...string) (*mcpDeps, *scriptedBackend) {
	t.Helper()
	t.Setenv("REIN_HOME", t.TempDir())
	be := &scriptedBackend{replies: replies}
	return &mcpDeps{
		ceiling: ceiling,
		steps:   4,
		newSpec: func(_ context.Context, tool string, _ bool) (*spec.Spec, error) {
			return &spec.Spec{Tool: tool, RootHelp: "usage: " + tool}, nil
		},
		newBackend: func() (planner.Backend, error) { return be, nil },
	}, be
}

func call(t *testing.T, d *mcpDeps, args string) (string, error) {
	t.Helper()
	return d.runIn(context.Background(), json.RawMessage(args))
}

func TestMCPRunReturnsAnswerAndTranscript(t *testing.T) {
	d, _ := mcpDepsFor(t, loop.ApproveSafe,
		`{"action":"run","argv":["true"],"risk":"safe","purpose":"probe"}`,
		`{"action":"answer","answer":"all good"}`,
	)
	got, err := call(t, d, `{"tool":"true","intent":"check"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "all good") {
		t.Errorf("answer should lead:\n%s", got)
	}
	if !strings.Contains(got, "--- transcript ---") || !strings.Contains(got, "$ true") {
		t.Errorf("transcript missing:\n%s", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("ANSI escapes leaked into the tool result:\n%s", got)
	}
}

// Headless runs have no one to ask, so a command above the granted level must
// stop the run without executing, and the stop must be reported as an
// outcome the caller can act on rather than as a failure.
func TestMCPMutatingCommandStopsWithoutRunning(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "created")
	d, be := mcpDepsFor(t, loop.ApproveCaution,
		`{"action":"run","argv":["touch","`+marker+`"],"risk":"caution"}`,
		`{"action":"answer","answer":"should not get here"}`,
	)
	got, err := call(t, d, `{"tool":"touch","intent":"make a file"}`)
	if err != nil {
		t.Fatalf("a gate stop is not an error: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("mutating command ran without approval")
	}
	if !strings.HasPrefix(got, "status: needs-approval") {
		t.Errorf("expected needs-approval outcome:\n%s", got)
	}
	// The ceiling allows "yes", so the caller is told to retry with it.
	if !strings.Contains(got, `approval "yes"`) {
		t.Errorf("should suggest retrying with the next level:\n%s", got)
	}
	if be.calls != 1 {
		t.Errorf("planner should not be consulted after the stop; calls = %d", be.calls)
	}
}

func TestMCPRefusesAboveCeiling(t *testing.T) {
	d, be := mcpDepsFor(t, loop.ApproveSafe)
	_, err := call(t, d, `{"tool":"git","intent":"x","approval":"auto"}`)
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("expected a ceiling refusal, got %v", err)
	}
	if be.calls != 0 {
		t.Error("nothing should run when the request is refused")
	}
	if _, err := call(t, d, `{"tool":"git","intent":"x","approval":"sometimes"}`); err == nil {
		t.Error("unknown approval value accepted")
	}
}

// When the ceiling itself is the blocker, the message must say so instead of
// suggesting a retry that would be refused.
func TestMCPStopAtCeilingSaysSo(t *testing.T) {
	d, _ := mcpDepsFor(t, loop.ApproveSafe,
		`{"action":"run","argv":["git","push","--force"],"risk":"danger"}`,
	)
	got, err := call(t, d, `{"tool":"git","intent":"force push"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ceiling") || !strings.Contains(got, "Gateway or direct MCP server with `--yes`") {
		t.Errorf("should explain the ceiling and how to raise it:\n%s", got)
	}
}

func TestMCPQuestionComesBackToCaller(t *testing.T) {
	d, _ := mcpDepsFor(t, loop.ApproveSafe,
		`{"action":"ask","question":"which branch?"}`,
	)
	got, err := call(t, d, `{"tool":"git","intent":"push it"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "status: needs-input") || !strings.Contains(got, "which branch?") {
		t.Errorf("question was not relayed:\n%s", got)
	}
}

func TestMCPBackendErrorIsToolError(t *testing.T) {
	d, _ := mcpDepsFor(t, loop.ApproveSafe)
	d.newBackend = func() (planner.Backend, error) { return nil, errors.New("no credentials") }
	if _, err := call(t, d, `{"tool":"git","intent":"x"}`); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("backend error should surface: %v", err)
	}
	if _, err := call(t, d, `{"tool":"git log","intent":"x"}`); err == nil {
		t.Error("a tool name with spaces should be rejected")
	}
	if _, err := call(t, d, `{"tool":"git"}`); err == nil {
		t.Error("missing intent should be rejected")
	}
}
