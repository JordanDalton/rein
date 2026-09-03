package risk

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		argv []string
		want Level
	}{
		{[]string{"git"}, Safe},
		{[]string{"git", "log", "-2"}, Safe},
		{[]string{"kubectl", "get", "pods", "-n", "staging"}, Safe},
		{[]string{"gh", "pr", "list"}, Safe},     // read verb found at depth 2
		{[]string{"gh", "pr", "merge"}, Caution}, // neither read nor write: pessimistic
		{[]string{"git", "commit", "-m", "x"}, Caution},
		{[]string{"git", "push"}, Caution},
		{[]string{"kubectl", "delete", "pod", "web-1"}, Danger},
		{[]string{"git", "remote", "remove", "origin"}, Danger},
		{[]string{"git", "reset", "--hard"}, Danger},
		{[]string{"git", "push", "--force"}, Danger},
		{[]string{"gh", "repo", "delete"}, Danger},

		// A resource named after a destructive verb must not escalate the
		// command that merely reads it.
		{[]string{"kubectl", "get", "pods", "delete"}, Safe},
		{[]string{"kubectl", "logs", "prune-job-1"}, Safe},

		// Flags that spell a destructive intent still escalate.
		{[]string{"gh", "cache", "--delete"}, Danger},
	}
	for _, c := range cases {
		if got := Classify(c.argv); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}

func TestParseDefaultsToCaution(t *testing.T) {
	for _, s := range []string{"", "unknown", "medium", "SAFE-ish"} {
		if got := Parse(s); got != Caution {
			t.Errorf("Parse(%q) = %v, want Caution", s, got)
		}
	}
	if Parse("DANGER") != Danger || Parse("safe") != Safe {
		t.Error("Parse failed on known levels")
	}
}

func TestMaxTakesHigherLevel(t *testing.T) {
	// The gate must not be talked down by a model that under-reports risk.
	if Max(Classify([]string{"kubectl", "delete", "ns", "prod"}), Parse("safe")) != Danger {
		t.Error("a model-declared safe label overrode a destructive command")
	}
}

// The wrapped binary can itself be the destructive verb — there is no
// subcommand to inspect in `rm -rf /tmp/x`.
func TestBinaryNameIsPartOfTheVerbPath(t *testing.T) {
	cases := []struct {
		argv []string
		want Level
	}{
		{[]string{"rm", "-f", "/tmp/x"}, Danger},
		{[]string{"/bin/rm", "/tmp/x"}, Danger},
		{[]string{"shred", "/tmp/x"}, Caution}, // unknown binary: pessimistic
		{[]string{"cat", "/etc/hosts"}, Safe},
		{[]string{"ls", "-la"}, Safe},
	}
	for _, c := range cases {
		if got := Classify(c.argv); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}

// A bare -f means "force" on some subcommands and "file" on others.
func TestShortForceFlag(t *testing.T) {
	cases := []struct {
		argv []string
		want Level
	}{
		{[]string{"git", "push", "-f"}, Danger},
		{[]string{"git", "clean", "-fd"}, Danger},
		{[]string{"grep", "-f", "patterns.txt", "file"}, Caution}, // -f is a file here
		{[]string{"git", "log", "-f"}, Safe},                      // read verb wins
	}
	for _, c := range cases {
		if got := Classify(c.argv); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}

// A list or dry-run flag makes an otherwise unknown command read-only, but
// never softens a write verb or a force flag.
func TestReadFlags(t *testing.T) {
	cases := []struct {
		argv []string
		want Level
	}{
		{[]string{"git", "tag", "-l"}, Safe},
		{[]string{"git", "branch", "--list", "feat/*"}, Safe},
		{[]string{"git", "push", "--dry-run"}, Safe},
		{[]string{"kubectl", "apply", "--dry-run=client", "-f", "x.yaml"}, Safe},
		{[]string{"git", "tag"}, Caution},
		{[]string{"git", "tag", "-d", "v1"}, Caution},
		{[]string{"git", "tag", "-l", "-f", "v1"}, Danger},                      // force still wins
		{[]string{"kubectl", "delete", "pod", "x", "--dry-run=client"}, Danger}, // write verb still wins
		{[]string{"rm", "--dry-run", "x"}, Danger},
	}
	for _, c := range cases {
		if got := Classify(c.argv); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}

// Reading a file that conventionally holds credentials is not destructive,
// but it is not something to run unattended: it lifts a read to Caution.
func TestSecretFilesNeedAHuman(t *testing.T) {
	cases := []struct {
		argv []string
		want Level
	}{
		{[]string{"cat", ".env"}, Caution},
		{[]string{"cat", "./config/.env.production"}, Caution},
		{[]string{"cat", "/Users/me/.aws/credentials"}, Caution},
		{[]string{"cat", "id_rsa"}, Caution},
		{[]string{"git", "show", "HEAD:.env"}, Caution},
		{[]string{"kubectl", "create", "secret", "generic", "x", "--from-env-file=.env"}, Caution},
		{[]string{"sops", "-d", "secrets.yaml"}, Caution},

		// Public halves and ordinary files stay read-only.
		{[]string{"cat", "id_rsa.pub"}, Safe},
		{[]string{"cat", "README.md"}, Safe},
		{[]string{"cat", ".env.example"}, Caution}, // matches .env.*; a human can wave it through
		{[]string{"cat"}, Safe},

		// Destruction still outranks disclosure.
		{[]string{"rm", ".env"}, Danger},
		{[]string{"git", "checkout", "-f", ".env"}, Danger},
	}
	for _, c := range cases {
		if got := Classify(c.argv); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
	if got := SecretOperand([]string{"cat", "a.txt", "b/.env"}); got != "b/.env" {
		t.Errorf("SecretOperand = %q, want b/.env", got)
	}
	if got := SecretOperand([]string{"cat", "--flag", "a.txt"}); got != "" {
		t.Errorf("SecretOperand = %q, want empty", got)
	}
}
