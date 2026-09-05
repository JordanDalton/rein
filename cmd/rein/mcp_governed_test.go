package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jordandalton/rein/internal/risk"
	"github.com/jordandalton/rein/internal/spec"
)

func TestGovernedSessionLoadsTheRegisteredAgentCredential(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("secure credential storage is currently implemented with macOS Keychain")
	}
	home := t.TempDir()
	t.Setenv("REIN_HOME", home)
	agent := cloudAgent{ID: "01JAGENT", Provider: "codex"}
	if err := saveCloudProfile(cloudProfile{ControlURL: "https://control.example"}); err != nil {
		t.Fatal(err)
	}
	if err := saveAgents(cloudAgents{Agents: []cloudAgent{agent}}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	trace := filepath.Join(home, "security-args")
	script := fmt.Sprintf("#!/bin/sh\nprintf 'agent-token\\n'\nprintf '%%s\\n' \"$@\" > %q\n", trace)
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	g, err := newMCPGoverned("codex")
	if err != nil {
		t.Fatal(err)
	}
	if g.caller != "codex" {
		t.Fatalf("caller = %q", g.caller)
	}
	args, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "cloud:https://control.example:01JAGENT") {
		t.Fatalf("agent credential account not requested: %s", args)
	}
}

func TestGovernedPolicyFailsClosed(t *testing.T) {
	for _, mode := range []string{"offline", "expired", "unpublished", "invalid", "unmatched"} {
		t.Run(mode, func(t *testing.T) {
			g := &mcpGoverned{caller: "test", request: func(_ context.Context, method, route string, body, out any) error {
				if route != "policy-bundles/latest" {
					t.Fatal("authorization proceeded")
				}
				if mode == "offline" {
					return errors.New("offline")
				}
				b := ciBundle{Version: 1, ExpiresAt: ciTime{time.Now().Add(time.Hour)}, Rules: []ciRule{{Effect: "allow"}}}
				switch mode {
				case "expired":
					b.ExpiresAt = ciTime{time.Now().Add(-time.Hour)}
				case "unpublished":
					b.Version = 0
				case "invalid":
					b.Rules[0].Effect = "oops"
				case "unmatched":
					b.Rules = nil
				}
				*out.(*ciBundle) = b
				return nil
			}}
			if err := g.authorize(context.Background(), "true", "test", []string{"true"}, risk.Safe); err == nil {
				t.Fatal("allowed invalid policy")
			}
		})
	}
}

func TestGovernedApprovalUsesExactCurrentArgv(t *testing.T) {
	var requested map[string]any
	g := &mcpGoverned{caller: "test", workDir: "/tmp/rein-project", request: func(_ context.Context, method, route string, body, out any) error {
		switch {
		case route == "policy-bundles/latest":
			*out.(*ciBundle) = ciBundle{Version: 1, ExpiresAt: ciTime{time.Now().Add(time.Hour)}, Rules: []ciRule{{Effect: "allow", Command: "status"}, {Effect: "require_approval"}}}
		case strings.HasPrefix(route, "approvals/check?"):
		case route == "approvals":
			requested = body.(map[string]any)
		default:
			t.Fatal(route)
		}
		return nil
	}}
	if err := g.authorize(context.Background(), "git", "task", []string{"git", "status"}, risk.Safe); err != nil {
		t.Fatal(err)
	}
	argv := []string{"git", "commit", "-m", "Jordan's change"}
	if err := g.authorize(context.Background(), "git", "task", argv, risk.Caution); err == nil {
		t.Fatal("missing approval allowed")
	}
	operation := requested["operation"].(map[string]any)
	if operation["cwd"] != "/tmp/rein-project" {
		t.Fatalf("gateway working directory missing from approval: %v", operation["cwd"])
	}
	if !reflect.DeepEqual(operation["argv"], argv) {
		t.Fatal(operation)
	}
	if requested["operation_hash"] != governedHash(operation) {
		t.Fatal("hash mismatch")
	}
	if governedHash(map[string]any{"argv": []string{"x", "a b"}}) == governedHash(map[string]any{"argv": []string{"x", "a", "b"}}) {
		t.Fatal("ambiguous argv")
	}
}

func TestGovernedSpecNeverDiscovers(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	d := &mcpDeps{caller: "test", newSpec: func(context.Context, string, bool) (*spec.Spec, error) {
		t.Fatal("discovery executed")
		return nil, nil
	}}
	for _, refresh := range []bool{false, true} {
		if _, err := d.toolSpec(context.Background(), "git", refresh); err == nil {
			t.Fatal("missing spec accepted")
		}
	}
}

func TestBlockAuditPreservesFailure(t *testing.T) {
	for _, offline := range []bool{false, true} {
		cause := errors.New("blocked by policy: denied")
		g := &mcpGoverned{caller: "codex", operation: map[string]any{"tool": "cat", "policy_version": 2}}
		g.request = func(_ context.Context, method, route string, body, out any) error {
			if method != "POST" || route != "audit-events" {
				t.Fatal(method, route)
			}
			payload := body.(map[string]any)
			if payload["event"] != "policy_denied" || payload["caller"] != "codex" {
				t.Fatal(payload)
			}
			metadata := payload["metadata"].(map[string]any)
			if metadata["executed"] != false || metadata["stage"] != "authorization" || metadata["reason"] != cause.Error() {
				t.Fatal(metadata)
			}
			if offline {
				return errors.New("offline")
			}
			return nil
		}
		err := g.recordBlock(context.Background(), "authorization", cause)
		if !errors.Is(err, cause) {
			t.Fatal("lost original block", err)
		}
		if offline && !strings.Contains(err.Error(), "could not be recorded") {
			t.Fatal(err)
		}
	}
}

func TestRequiredSpecsFailWithoutDiscovery(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	for _, name := range []string{"cat", "cat,git", "../cat", "cat;touch marker", "cat,"} {
		if err := checkRequiredSpecs(context.Background(), name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	err := checkRequiredSpecs(context.Background(), "cat")
	if !strings.Contains(err.Error(), "rein spec cat") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Fatal(err)
	}
}
