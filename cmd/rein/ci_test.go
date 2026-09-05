package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCIDecisionFailsClosed(t *testing.T) {
	b := ciBundle{Version: 1, ExpiresAt: ciTime{time.Now().Add(time.Hour)}, Rules: []ciRule{{Effect: "allow", Caller: "github-actions", Tool: "git", Command: "status"}}}
	if effect, err := ciDecision(b, "github-actions", "", []string{"git", "status"}); err != nil || effect != "allow" {
		t.Fatalf("%s %v", effect, err)
	}
	for _, argv := range [][]string{{"git", "push"}, {"bash", "-c", "git status"}} {
		if effect, _ := ciDecision(b, "github-actions", "", argv); effect != "deny" {
			t.Fatal("unmatched command allowed")
		}
	}
	b.ExpiresAt = ciTime{time.Now().Add(-time.Second)}
	if _, err := ciDecision(b, "github-actions", "", []string{"git", "status"}); err == nil {
		t.Fatal("expired bundle accepted")
	}
	b.ExpiresAt = ciTime{time.Now().Add(time.Hour)}
	b.Rules[0].Effect = "unknown"
	if _, err := ciDecision(b, "github-actions", "", []string{"git", "status"}); err == nil {
		t.Fatal("unknown effect accepted")
	}
}

func TestCIDecisionMatchesExactPolicyAccessLevel(t *testing.T) {
	cases := []struct {
		access string
		argv   []string
	}{
		{"read", []string{"git", "status"}},
		{"write", []string{"git", "commit", "-m", "change"}},
		{"destructive", []string{"git", "reset", "--hard"}},
	}
	for _, test := range cases {
		t.Run(test.access, func(t *testing.T) {
			bundle := ciBundle{Version: 1, ExpiresAt: ciTime{time.Now().Add(time.Hour)}, Rules: []ciRule{
				{Effect: "allow", Caller: "codex", Tool: "git", Access: test.access},
				{Effect: "deny", Caller: "*", Tool: "*", Access: "any"},
			}}
			if effect, err := ciDecision(bundle, "codex", "", test.argv); err != nil || effect != "allow" {
				t.Fatalf("matching %s operation = %q, %v", test.access, effect, err)
			}
			for _, other := range cases {
				if other.access == test.access {
					continue
				}
				if effect, err := ciDecision(bundle, "codex", "", other.argv); err != nil || effect != "deny" {
					t.Fatalf("%s rule matched %s operation: %q, %v", test.access, other.access, effect, err)
				}
			}
		})
	}
}

func TestCIEndToEndWithControlPlane(t *testing.T) {
	for _, scenario := range []string{"allow", "deny", "approval", "approval-no-wait", "auth-failure", "policy-failure", "audit-start-failure", "audit-outcome-failure", "command-failure"} {
		t.Run(scenario, func(t *testing.T) {
			executed := false
			events := []string{}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer rein_wrk_test" {
					t.Error("missing workload credential")
				}
				switch r.URL.Path {
				case "/api/v1/rein/status":
					if scenario == "auth-failure" {
						w.WriteHeader(401)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"credential_type": "workload", "provider": "github-actions"})
				case "/api/v1/rein/policy-bundles/latest":
					if scenario == "policy-failure" {
						w.WriteHeader(503)
						return
					}
					effect := "allow"
					if scenario == "deny" {
						effect = "deny"
					}
					if strings.HasPrefix(scenario, "approval") {
						effect = "require_approval"
					}
					json.NewEncoder(w).Encode(map[string]any{"version": 1, "expires_at": time.Now().Add(time.Hour).UTC().Format("2006-01-02 15:04:05"), "rules": []ciRule{{Effect: effect, Caller: "github-actions", Tool: "git", Command: "status"}}})
				case "/api/v1/rein/audit-events":
					var payload struct{ Event string }
					json.NewDecoder(r.Body).Decode(&payload)
					events = append(events, payload.Event)
					if (scenario == "audit-start-failure" && payload.Event == "execution.started") || (scenario == "audit-outcome-failure" && payload.Event == "execution.completed") {
						w.WriteHeader(503)
						return
					}
					w.WriteHeader(202)
				case "/api/v1/rein/approvals":
					json.NewEncoder(w).Encode(map[string]string{"id": "approval-1"})
				case "/api/v1/rein/approvals/check":
					json.NewEncoder(w).Encode(map[string]bool{"approved": true})
				default:
					t.Error("unexpected route", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			originalClient, originalExecute := ciClient, ciExecute
			ciClient = server.Client()
			ciExecute = func(cmd *exec.Cmd) error {
				executed = true
				for _, env := range cmd.Env {
					if strings.HasPrefix(env, "REIN_WORKLOAD_TOKEN=") || strings.HasPrefix(env, "ACTIONS_ID_TOKEN_REQUEST_TOKEN=") {
						t.Error("credential leaked to child")
					}
				}
				if cmd.Args[0] != "git" || cmd.Args[1] != "status" {
					t.Error("wrong argv")
				}
				if scenario == "command-failure" {
					return errors.New("failed")
				}
				return nil
			}
			defer func() { ciClient = originalClient; ciExecute = originalExecute }()
			t.Setenv("REIN_CONTROL_URL", server.URL)
			t.Setenv("REIN_WORKLOAD_TOKEN", "rein_wrk_test")
			t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "github-secret")
			args := []string{"run", "--", "git", "status"}
			if scenario == "approval" {
				args = []string{"run", "--approval-timeout", "1s", "--", "git", "status"}
			}
			err := cmdCI(context.Background(), args)
			wantExecuted := scenario == "allow" || scenario == "approval" || scenario == "audit-outcome-failure" || scenario == "command-failure"
			if executed != wantExecuted {
				t.Fatalf("executed=%v, err=%v", executed, err)
			}
			wantOK := scenario == "allow" || scenario == "approval"
			if (err == nil) != wantOK {
				t.Fatalf("unexpected result: %v", err)
			}
			if scenario == "deny" && (len(events) != 1 || events[0] != "execution.blocked") {
				t.Fatal("denial not audited")
			}
		})
	}
}

func TestCIRejectsInsecureOrigin(t *testing.T) {
	t.Setenv("REIN_WORKLOAD_TOKEN", "secret")
	for _, origin := range []string{"http://rein.test", "https://user:pass@rein.test", "https://rein.test/path", "https://rein.test?x=y"} {
		t.Setenv("REIN_CONTROL_URL", origin)
		if err := cmdCI(context.Background(), []string{"check"}); err == nil {
			t.Fatal("accepted insecure origin")
		}
	}
}
