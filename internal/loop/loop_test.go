package loop

import (
	"bytes"
	"context"
	"errors"
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
	last    string // the most recent user message, so a test can see what the planner saw
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Complete(_ context.Context, _, user string) (string, error) {
	f.last = user
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

// With no terminal, a question from the planner is returned to the caller
// rather than swallowed, and a gate stop is identifiable as such.
func TestHeadlessRunReportsWhatItNeeds(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())

	be := &fakeBackend{replies: []string{`{"action":"ask","question":"which remote?"}`}}
	_, err := Run(context.Background(), newConfig(t, "git", be, ""))
	var needs *NeedsInputError
	if !errors.As(err, &needs) || needs.Question != "which remote?" {
		t.Errorf("expected NeedsInputError carrying the question, got %v", err)
	}

	be = &fakeBackend{replies: []string{`{"action":"run","argv":["git","push"],"risk":"caution"}`}}
	_, err = Run(context.Background(), newConfig(t, "git", be, ""))
	if !errors.Is(err, ErrNoTerminal) {
		t.Errorf("expected ErrNoTerminal at the gate, got %v", err)
	}
}

// A command that fails is not the end of the run: the loop must go back to
// the planner with the exit code and stderr in hand so it can fix the command.
func TestFailedCommandIsFedBackToPlanner(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["git","--no-such-flag-xyz"],"risk":"safe"}`,
		`{"action":"answer","answer":"recovered"}`,
	}}
	cfg := newConfig(t, "git", be, "")

	answer, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "recovered" {
		t.Errorf("answer = %q", answer)
	}
	if be.calls != 2 {
		t.Fatalf("planner should be consulted again after a failure; calls = %d", be.calls)
	}
	for _, want := range []string{"exit: 129", "no-such-flag-xyz"} {
		if !strings.Contains(be.last, want) {
			t.Errorf("planner was not shown %q after the failure; it saw:\n%s", want, be.last)
		}
	}
}

// Reading a credentials file is not destructive, but it is not something to
// run unattended either: the default approval mode must stop and ask, and
// the prompt must say why even when the model called it safe.
func TestSecretFileReadStopsForAHuman(t *testing.T) {
	t.Setenv("REIN_HOME", t.TempDir())
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["cat",".env"],"risk":"safe"}`,
		`{"action":"answer","answer":"declined"}`,
	}}
	cfg := newConfig(t, "cat", be, "n\n")
	out := cfg.Out.(*bytes.Buffer)

	answer, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "declined" {
		t.Errorf("answer = %q", answer)
	}
	if !strings.Contains(out.String(), "run this?") {
		t.Errorf("expected an approval prompt for cat .env; output:\n%s", out)
	}
	if !strings.Contains(out.String(), "usually holds credentials") {
		t.Errorf("expected a synthesised consequence; output:\n%s", out)
	}
	if !strings.Contains(be.last, "declined") {
		t.Errorf("planner was not told the command was declined; it saw:\n%s", be.last)
	}
}

// Whatever a command prints, credentials are masked before the terminal, the
// model, or the run log sees them.
func TestSecretsAreMaskedBeforeAnyoneSeesThem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REIN_HOME", home)
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("SLACK_TOKEN=xoxb-FAKE-FAKE-secretsecret\nNAME=tackle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	be := &fakeBackend{replies: []string{
		`{"action":"run","argv":["cat","` + env + `"],"risk":"safe"}`,
		`{"action":"answer","answer":"done"}`,
	}}
	cfg := newConfig(t, "cat", be, "y\n") // a secret file: the gate asks
	out := cfg.Out.(*bytes.Buffer)

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	logs, _ := filepath.Glob(filepath.Join(home, "runs", "*", "*.log"))
	if len(logs) != 1 {
		t.Fatalf("expected one run log, found %v", logs)
	}
	archived, _ := os.ReadFile(logs[0])
	for who, text := range map[string]string{"terminal": out.String(), "planner": be.last, "run log": string(archived)} {
		if strings.Contains(text, "secretsecret") {
			t.Errorf("the %s saw the unmasked token:\n%s", who, text)
		}
	}
	// The model and the log get the masked contents; the terminal gets
	// nothing but a summary, since the user asked about the file, not for it.
	for who, text := range map[string]string{"planner": be.last, "run log": string(archived)} {
		if !strings.Contains(text, "xoxb…[redacted") {
			t.Errorf("the %s did not see the masked form:\n%s", who, text)
		}
	}
	if strings.Contains(out.String(), "xoxb") || strings.Contains(out.String(), "NAME=tackle") {
		t.Errorf("terminal showed the file contents:\n%s", out)
	}
	if !strings.Contains(out.String(), "withheld from the terminal (2 line(s), 1 secret(s) masked)") {
		t.Errorf("terminal was not told what was withheld:\n%s", out)
	}
	if info, _ := os.Stat(logs[0]); info.Mode().Perm() != 0o600 {
		t.Errorf("run log mode = %o, want 600", info.Mode().Perm())
	}
}
