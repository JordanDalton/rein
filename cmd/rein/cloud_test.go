package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCloudURL(t *testing.T) {
	t.Setenv("REIN_CONTROL_URL", "http://rein-control.test/")
	if got := cloudURL(""); got != "http://rein-control.test" {
		t.Fatalf("cloudURL() = %q", got)
	}
	if got := cloudURL("https://other.test/"); got != "https://other.test" {
		t.Fatalf("cloudURL(override) = %q", got)
	}
}

func TestFormatCloudStatus(t *testing.T) {
	for _, team := range []string{"", "   ", "jordan-daltons-team"} {
		p := cloudProfile{ControlURL: "https://custom.example", Organization: "Jordan Dalton's Team", TeamSlug: team, User: "Jordan", DeviceName: "Mac"}
		got := formatCloudStatus(p)
		for _, expected := range []string{"Control URL: https://custom.example\n", "Organization: Jordan Dalton's Team\n", "User: Jordan\n", "Device: Mac\n", "Last sync: just now\n"} {
			if !strings.Contains(got, expected) {
				t.Errorf("missing %q in %q", expected, got)
			}
		}
		if strings.TrimSpace(team) == "" {
			if strings.Contains(got, "\nTeam:") {
				t.Errorf("empty team displayed: %q", got)
			}
		} else if !strings.Contains(got, "Team: "+team+"\n") {
			t.Errorf("team missing: %q", got)
		}
	}
}

func TestHelpUsesReinControlBranding(t *testing.T) {
	if !strings.Contains(usage, "Rein Control") || strings.Contains(usage, "Rein Cloud") {
		t.Fatal("help must use Rein Control branding")
	}
}

func TestRemoteLoginTunnel(t *testing.T) {
	if got := remoteLoginTunnel(45943, "jordan@openclaw"); got != "ssh -N -L 45943:127.0.0.1:45943 jordan@openclaw" {
		t.Fatalf("remoteLoginTunnel() = %q", got)
	}
	if got := remoteLoginTunnel(45943, ""); got != "ssh -N -L 45943:127.0.0.1:45943 <user>@<remote-host>" {
		t.Fatalf("remoteLoginTunnel() placeholder = %q", got)
	}
}

func TestLocalPolicyMetadataOnlyMatchesApplicableApprovalRules(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	policy := `{"version":7,"rules":[{"effect":"require_approval","caller":"claude-code","tool":"kubectl","environment":"production","access":"write"}]}`
	if err := os.WriteFile(filepath.Join(os.Getenv("REIN_HOME"), "policy.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	if got := localPolicyMetadata("claude-code", "herd", "List sites", "herd sites"); got != nil {
		t.Fatalf("unrelated tool unexpectedly matched policy: %#v", got)
	}
	got := localPolicyMetadata("claude-code", "kubectl", "restart deployment in production", "kubectl rollout restart deployment/api -n production")
	if got == nil || got["effect"] != "require_approval" {
		t.Fatalf("matching approval rule not returned: %#v", got)
	}
	if !strings.Contains(got["reason"].(string), "approval") {
		t.Fatalf("missing policy reason: %#v", got)
	}
}

func TestCloudProfileRoundTrip(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	want := cloudProfile{
		ControlURL: "https://reincontrol.com", Organization: "Acme", User: "Jordan",
		DeviceID: "01JDEVICE", DeviceName: "Jordan's Mac", LastContact: time.Now().UTC().Round(time.Second),
	}
	if err := saveCloudProfile(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadCloudProfile()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != want {
		t.Fatalf("loadCloudProfile() = %#v, want %#v", got, want)
	}
}

func TestFetchCloudStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/rein/status" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer device-token" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"organization":"Acme","user":"Jordan","device_id":"01JDEVICE","device_name":"Jordan's Mac"}`))
	}))
	defer server.Close()

	got, err := fetchCloudStatus(context.Background(), server.URL, "device-token")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceID != "01JDEVICE" || got.Organization != "Acme" {
		t.Fatalf("identity = %#v", got)
	}
}

func TestFetchCloudStatusRejectsIncompleteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"organization":"Acme"}`))
	}))
	defer server.Close()

	if _, err := fetchCloudStatus(context.Background(), server.URL, "device-token"); err == nil {
		t.Fatal("expected incomplete response error")
	}
}

func TestCloudDeleteTreatsAnAlreadyMissingResourceAsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer device-token" {
			t.Error("missing bearer token")
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	if err := cloudDelete(context.Background(), server.URL+"/agent", "device-token"); err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}
}

func TestEnsureAgentCredentialRepairsStaleRegistration(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("secure credential storage is currently implemented with macOS Keychain")
	}
	home := t.TempDir()
	t.Setenv("REIN_HOME", home)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"new-agent","name":"claude-code","provider":"claude-code","token":"new-agent-token"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	if err := saveCloudProfile(cloudProfile{ControlURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := saveAgents(cloudAgents{Agents: []cloudAgent{{ID: "old-agent", Name: "claude-code", Provider: "claude-code"}}}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	trace := filepath.Join(home, "security-args")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  *find-generic-password*old-agent*) exit 44 ;;
  *find-generic-password*) printf 'device-token\n' ;;
esac
exit 0
`, trace)
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	if err := ensureAgentCredential(context.Background(), "claude-code"); err != nil {
		t.Fatal(err)
	}
	agents, err := loadAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents.Agents) != 1 || agents.Agents[0].ID != "new-agent" {
		t.Fatalf("stale registration was not replaced: %#v", agents.Agents)
	}
	if strings.Join(requests, ",") != "DELETE /api/v1/rein/agents/old-agent,POST /api/v1/rein/agents" {
		t.Fatalf("unexpected repair requests: %#v", requests)
	}
	securityArgs, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(securityArgs), "add-generic-password") || !strings.Contains(string(securityArgs), "new-agent") {
		t.Fatalf("replacement credential was not stored: %s", securityArgs)
	}
}
