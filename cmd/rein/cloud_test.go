package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
