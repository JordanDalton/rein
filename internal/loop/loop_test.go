package loop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordandalton/rein/internal/planner"
	"github.com/jordandalton/rein/internal/spec"
)

// fakeBackend replays a fixed script of plans and records how many times it was
// consulted, so a test can tell "the loop rejected this and re-planned" from
// "the loop stopped".
type fakeBackend struct {
	replies []string
	calls   int
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Complete(_ context.Context, _, _ string) (string, error) {
	if f.calls >= len(f.replies) {
		return `{"action":"answer","answer":"out of scripted replies"}`, nil
	}
	r := f.replies[f.calls]
	f.calls++
	return r, nil
}

func newConfig(t *testing.T, tool string, be planner.Backend, in string) Config {
	t.Helper()
	return Config{
		Spec:     &spec.Spec{Tool: tool, RootHelp: "usage: " + tool},
		Backend:  be,
		Intent:   "do the thing",
		MaxSteps: 4,
		Out:      &bytes.Buffer{},
		In:       strings.NewReader(in),
	}
}

// rein must only ever exec the binary it was pointed at, whatever the
// model asks for.
func TestForeignBinaryIsRejected(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["touch","` + marker + `"],"risk":"safe"}`,
		`{"action":"answer","answer":"done"}`,
	}}
	cfg := newConfig(t, "git", be, "")

	answer, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Errorf("answer = %q", answer)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command for a different binary was executed")
	}
	if be.calls != 2 {
		t.Errorf("expected the loop to re-plan after rejecting; backend calls = %d", be.calls)
	}
	if out := cfg.Out.(*bytes.Buffer).String(); !strings.Contains(out, "only runs \"git\"") {
		t.Errorf("rejection was not explained to the user:\n%s", out)
	}
}

// A declined command must not run, and the refusal must be fed back so the
// planner can try something else.
func TestDeclinedCommandDoesNotRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "created")
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["touch","` + marker + `"],"risk":"caution"}`,
		`{"action":"answer","answer":"user said no"}`,
	}}
	cfg := newConfig(t, "touch", be, "n\n")

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a declined command was executed anyway")
	}
}

// The same command runs once approved — proving the gate, not a broken
// executor, is what stopped it above.
func TestApprovedCommandRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "created")
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["touch","` + marker + `"],"risk":"caution"}`,
		`{"action":"answer","answer":"created"}`,
	}}
	cfg := newConfig(t, "touch", be, "y\n")

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("approved command did not run: %v", err)
	}
}

// A model that labels a destructive command "safe" must not thereby skip the
// prompt.
func TestUnderReportedRiskStillPrompts(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["rm","-f","` + victim + `"],"risk":"safe"}`,
		`{"action":"answer","answer":"stopped"}`,
	}}
	// ApproveCaution would auto-run a merely-mutating command; this one is
	// destructive, so it must still ask — and the scripted answer is "no".
	cfg := newConfig(t, "rm", be, "n\n")
	cfg.Approval = ApproveCaution

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("a destructive command mislabelled safe was executed without asking")
	}
}

// With no way to ask, the loop must fail closed rather than assume yes.
func TestNoInputFailsClosed(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "created")
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["touch","` + marker + `"],"risk":"caution"}`,
	}}
	cfg := newConfig(t, "touch", be, "") // EOF immediately

	_, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected an error when approval input is unavailable")
	}
	if !strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("error should tell the user how to proceed, got: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("command ran despite having no way to get approval")
	}
}

func TestDryRunPlansWithoutExecuting(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "created")
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["touch","` + marker + `"],"risk":"safe"}`,
	}}
	cfg := newConfig(t, "touch", be, "")
	cfg.DryRun = true

	answer, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "touch") {
		t.Errorf("dry run should report the proposed command, got %q", answer)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("dry run executed the command")
	}
}

func TestStepBudgetIsEnforced(t *testing.T) {
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["true"],"risk":"safe"}`,
		`{"action":"run","argv":["true"],"risk":"safe"}`,
		`{"action":"run","argv":["true"],"risk":"safe"}`,
	}}
	cfg := newConfig(t, "true", be, "")
	cfg.MaxSteps = 2

	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("expected the loop to stop at the step budget")
	}
	if be.calls != 2 {
		t.Errorf("backend called %d times, want 2", be.calls)
	}
}

// The gate is only a real check if the person reading it can tell what they are
// agreeing to, so the plain-language consequence must reach the screen before
// the prompt does.
func TestConsequenceIsShownBeforeTheGate(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "created")
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["touch","` + marker + `"],"risk":"danger",
		  "purpose":"Overwrite the file",
		  "consequence":"This permanently replaces your saved work. It cannot be undone."}`,
		`{"action":"answer","answer":"stopped"}`,
	}}
	cfg := newConfig(t, "touch", be, "n\n")

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	out := cfg.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "cannot be undone") {
		t.Fatalf("consequence was not shown:\n%s", out)
	}
	if strings.Index(out, "cannot be undone") > strings.Index(out, "run this?") {
		t.Error("consequence appeared after the prompt; it must come first")
	}
}

// A read-only command needs no warning, and one attached to it must not add
// noise that dulls the warnings that matter.
func TestConsequenceSuppressedForSafeCommands(t *testing.T) {
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["true"],"risk":"safe","consequence":"Nothing happens."}`,
		`{"action":"answer","answer":"done"}`,
	}}
	cfg := newConfig(t, "true", be, "")

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if out := cfg.Out.(*bytes.Buffer).String(); strings.Contains(out, "Nothing happens") {
		t.Errorf("a consequence was shown for a read-only command:\n%s", out)
	}
}

// A plan with no consequence must still run: weaker models omit the field, and
// losing the command entirely would be worse than losing the warning.
func TestMissingConsequenceDoesNotBlockTheGate(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "created")
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["touch","` + marker + `"],"risk":"caution"}`,
		`{"action":"answer","answer":"done"}`,
	}}
	cfg := newConfig(t, "touch", be, "y\n")

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("approved command did not run: %v", err)
	}
}

func TestWrapTextBreaksOnSpaces(t *testing.T) {
	lines := wrapText("the quick brown fox jumps over the lazy dog", 12)
	for _, l := range lines {
		if len(l) > 12 {
			t.Errorf("line %q exceeds the width", l)
		}
		if strings.HasPrefix(l, " ") || strings.HasSuffix(l, " ") {
			t.Errorf("line %q has stray padding", l)
		}
	}
	if strings.Join(lines, " ") != "the quick brown fox jumps over the lazy dog" {
		t.Errorf("wrapping lost or reordered words: %v", lines)
	}
	if wrapText("   ", 10) != nil {
		t.Error("blank text should wrap to nothing")
	}
}
