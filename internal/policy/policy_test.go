package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jordandalton/rein/internal/risk"
)

func TestRequiresApprovalMatchesReadOperation(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	policy := `{"version":7,"rules":[{"effect":"require_approval","caller":"claude-code","tool":"herd","access":"any"}]}`
	if err := os.WriteFile(filepath.Join(os.Getenv("REIN_HOME"), "policy.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	if !RequiresApproval("claude-code", "list sites", []string{"herd", "sites"}, risk.Caution) {
		t.Fatal("matching require_approval rule was not enforced")
	}
	if RequiresApproval("other-agent", "list sites", []string{"herd", "sites"}, risk.Caution) {
		t.Fatal("rule matched the wrong caller")
	}
}

func TestRequiresApprovalHonorsWriteAccess(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	policy := `{"rules":[{"effect":"require_approval","caller":"claude-code","tool":"herd","access":"write"}]}`
	if err := os.WriteFile(filepath.Join(os.Getenv("REIN_HOME"), "policy.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	if RequiresApproval("claude-code", "list sites", []string{"herd", "sites"}, risk.Safe) {
		t.Fatal("write-only rule matched a safe operation")
	}
	if !RequiresApproval("claude-code", "remove site", []string{"herd", "remove", "site"}, risk.Caution) {
		t.Fatal("write-only rule did not match a caution operation")
	}
}

func TestPolicyAccessLevelsMatchExactly(t *testing.T) {
	for _, test := range []struct {
		access string
		level  risk.Level
	}{
		{"read", risk.Safe},
		{"write", risk.Caution},
		{"destructive", risk.Danger},
	} {
		if !matches("codex", "git", "", "", test.access, "codex", "", []string{"git", "operation"}, test.level) {
			t.Errorf("%s did not match %s", test.access, test.level)
		}
		if test.level != risk.Safe && matches("codex", "git", "", "", "read", "codex", "", []string{"git", "operation"}, test.level) {
			t.Errorf("read rule matched %s", test.level)
		}
	}
}

func TestWildcardDefaultDenyWithExplicitAllow(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	policy := `{"rules":[{"effect":"allow","caller":"claude-code","tool":"herd","access":"any"},{"effect":"deny","caller":"*","tool":"*","access":"any"}]}`
	if err := os.WriteFile(filepath.Join(os.Getenv("REIN_HOME"), "policy.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckIntent("claude-code", "list sites", []string{"herd", "sites"}, risk.Safe); err != nil {
		t.Fatalf("explicit wildcard allow was denied: %v", err)
	}
	if err := CheckIntent("claude-code", "list repos", []string{"gh", "repo", "list"}, risk.Safe); err == nil {
		t.Fatal("catch-all deny did not block an unlisted tool")
	}
}

func TestWildcardRequireApprovalMetadataMatches(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	policy := `{"version":8,"rules":[{"effect":"require_approval","caller":"claude-*","tool":"herd","command":"sites *","access":"any"}]}`
	if err := os.WriteFile(filepath.Join(os.Getenv("REIN_HOME"), "policy.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	if !RequiresApproval("claude-code", "list sites", []string{"herd", "sites", "--json"}, risk.Safe) {
		t.Fatal("wildcard approval rule did not match")
	}
}
