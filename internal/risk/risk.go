// Package risk classifies a proposed argv so rein knows what it can run
// unattended and what needs a human.
//
// The classifier is deliberately pessimistic: anything it does not recognise is
// Caution, never Safe. It is a gate, not an oracle — the model also declares a
// risk level, and the loop takes the higher of the two.
package risk

import (
	"path/filepath"
	"strings"
)

type Level int

const (
	Safe Level = iota
	Caution
	Danger
)

func (l Level) String() string {
	switch l {
	case Safe:
		return "safe"
	case Danger:
		return "danger"
	default:
		return "caution"
	}
}

// Parse maps a model-declared level onto ours, defaulting to Caution.
func Parse(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "safe", "read", "readonly", "read-only":
		return Safe
	case "danger", "dangerous", "destructive":
		return Danger
	default:
		return Caution
	}
}

func Max(a, b Level) Level {
	if a > b {
		return a
	}
	return b
}

// Access maps an execution risk level to the policy editor's access names.
func Access(level Level) string {
	switch level {
	case Safe:
		return "read"
	case Danger:
		return "destructive"
	default:
		return "write"
	}
}

// readVerbs are commands that observe without changing anything.
var readVerbs = map[string]bool{
	"get": true, "list": true, "ls": true, "show": true, "describe": true,
	"view": true, "cat": true, "read": true, "status": true, "log": true,
	"logs": true, "version": true, "help": true, "search": true, "find": true,
	"diff": true, "explain": true, "inspect": true, "top": true, "info": true,
	"history": true, "tree": true, "check": true, "validate": true, "lint": true,
	"plan": true, "print": true, "query": true, "count": true, "stat": true,
	"whoami": true, "ping": true, "test": true, "blame": true, "annotate": true,
}

// writeVerbs are commands that can destroy work or state. Anything here needs a
// human even in --yes mode.
var writeVerbs = map[string]bool{
	"delete": true, "destroy": true, "rm": true, "remove": true, "drop": true,
	"purge": true, "prune": true, "truncate": true, "reset": true, "revert": true,
	"rollback": true, "kill": true, "terminate": true, "shutdown": true,
	"uninstall": true, "wipe": true, "erase": true, "format": true, "evict": true,
	"cordon": true, "drain": true, "rmdir": true, "unlink": true, "clean": true,
}

// forceFlags turn an otherwise ordinary command into a destructive one.
var forceFlags = map[string]bool{
	"--force": true, "--hard": true, "-D": true, "--no-preserve-root": true,
	"--force-with-lease": true, "--purge": true,
}

// readFlags turn a command that would otherwise be unknown into a read-only
// one: `git tag -l`, `git push --dry-run`. They never soften a write verb or
// a force flag, which are checked first.
var readFlags = map[string]bool{
	"--list": true, "-l": true, "--dry-run": true, "--help": true, "-h": true,
	"--version": true,
}

// secretFiles are base names that conventionally hold credentials. Reading
// one is not destructive, but it discloses: whatever it prints reaches the
// terminal, the model backend, and the run log. That is a decision for a
// human, so any such operand lifts a read-only command to Caution. Write
// verbs and force flags are checked first and still win.
var secretFiles = []string{
	".env", ".env.*", "*.env", ".envrc",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
	".netrc", "_netrc", ".npmrc", ".pypirc", ".pgpass", ".my.cnf", ".htpasswd",
	".git-credentials", "credentials", "credentials.json",
	"secrets.*", "*.secret", "*.secrets", "secrets",
}

// SecretOperand returns the first argument of argv that names a file which
// usually holds credentials, or "" if there is none. Flag values are checked
// too (--file=.env), flag names are not.
func SecretOperand(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	for _, a := range argv[1:] {
		v := a
		if strings.HasPrefix(a, "-") {
			_, val, ok := strings.Cut(a, "=")
			if !ok {
				continue
			}
			v = val
		}
		if isSecretFile(v) {
			return v
		}
	}
	return ""
}

func isSecretFile(path string) bool {
	base := filepath.Base(filepath.Clean(path))
	if base == "." || base == "/" {
		return false
	}
	for _, pat := range secretFiles {
		if ok, _ := filepath.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// verbPathDepth is how many leading bare tokens can form the subcommand path
// ("git remote remove"). Beyond that, tokens are operands and must not
// escalate the classification — a pod literally named "delete-me" is not a
// destructive command.
const verbPathDepth = 2

// forceProneVerbs are subcommands where a bare -f means "force" rather than
// "file". Listing them explicitly avoids escalating genuinely harmless uses
// like `grep -f patterns.txt` or `tar -f archive.tar`.
var forceProneVerbs = map[string]bool{
	"push": true, "reset": true, "checkout": true, "clean": true,
	"branch": true, "tag": true, "rm": true, "remove": true, "delete": true,
	"prune": true, "stop": true, "restart": true, "kill": true, "unmount": true,
}

// shortFlagHasF reports whether a token is a bundled short-flag group
// containing -f, e.g. "-f" or "-rf".
func shortFlagHasF(a string) bool {
	if len(a) < 2 || a[0] != '-' || a[1] == '-' {
		return false
	}
	for _, r := range a[1:] {
		if r == 'f' {
			return true
		}
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return false
}

// Classify inspects a full argv (including the binary at argv[0]).
func Classify(argv []string) Level {
	if len(argv) == 0 {
		return Danger
	}

	// The binary itself can be the verb: `rm -rf x` has no subcommand, and
	// wrapping `rm` means every invocation is destructive.
	verbs := []string{filepath.Base(argv[0])}
	sawF, sawReadFlag := false, false

	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") {
			if forceFlags[a] {
				return Danger
			}
			// --force-delete, --delete-local-refs, and friends.
			if writeVerbs[strings.TrimLeft(a, "-")] {
				return Danger
			}
			if shortFlagHasF(a) {
				sawF = true
			}
			if name, _, _ := strings.Cut(a, "="); readFlags[name] {
				sawReadFlag = true
			}
			continue
		}
		if len(verbs) <= verbPathDepth {
			verbs = append(verbs, a)
		}
	}

	level := Caution
	sawReadVerb := false
	for _, v := range verbs {
		if writeVerbs[v] {
			return Danger
		}
		if sawF && forceProneVerbs[v] {
			return Danger
		}
		if readVerbs[v] {
			sawReadVerb = true
		}
	}
	if sawReadVerb || sawReadFlag {
		level = Safe
	}
	if len(verbs) == 1 {
		// Bare invocation, e.g. `kubectl` or `git`: prints usage, harmless.
		return Safe
	}
	// Disclosure is its own axis: `cat .env` changes nothing, but it is not
	// something to run unattended either.
	if level == Safe && SecretOperand(argv) != "" {
		level = Caution
	}
	return level
}
