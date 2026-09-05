package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jordandalton/rein/internal/risk"
	"github.com/jordandalton/rein/internal/spec"
)

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
	g := &mcpGoverned{caller: "test", request: func(_ context.Context, method, route string, body, out any) error {
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
