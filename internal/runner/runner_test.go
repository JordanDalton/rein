package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestElideKeepsHeadAndTail(t *testing.T) {
	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, "line")
	}
	lines[0] = "HEADER"
	lines[499] = "TOTAL"
	out, elided := elide(strings.Join(lines, "\n"), 20)
	if !elided {
		t.Fatal("expected elision")
	}
	if !strings.HasPrefix(out, "HEADER") || !strings.HasSuffix(out, "TOTAL") {
		t.Errorf("elide dropped the head or tail:\n%s", out)
	}
	if !strings.Contains(out, "lines elided") {
		t.Error("elide did not mark the gap")
	}
	if n := len(strings.Split(out, "\n")); n > 21 {
		t.Errorf("elide returned %d lines, want <= 21", n)
	}
}

func TestElideLeavesShortOutputAlone(t *testing.T) {
	if out, elided := elide("a\nb\nc", 20); elided || out != "a\nb\nc" {
		t.Errorf("got %q elided=%v", out, elided)
	}
}

func TestQuoteIsUnambiguous(t *testing.T) {
	got := Quote([]string{"git", "log", "--pretty=format:%h %s", "a b"})
	want := `git log '--pretty=format:%h %s' 'a b'`
	if got != want {
		t.Errorf("Quote() = %q, want %q", got, want)
	}
}

func TestRunCapturesExitCodeAndStreams(t *testing.T) {
	res, err := Run(context.Background(), []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if res.Stdout != "out" || res.Stderr != "err" {
		t.Errorf("stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestRunStripsANSI(t *testing.T) {
	res, err := Run(context.Background(), []string{"printf", "\x1b[31mred\x1b[0m"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "red" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "red")
	}
}

func TestRunTimesOutInsteadOfHanging(t *testing.T) {
	res, err := Run(context.Background(), []string{"sleep", "10"}, Options{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut")
	}
}

func TestRunClosesStdinSoToolsDoNotBlock(t *testing.T) {
	// A tool that reads stdin must hit EOF immediately rather than wait for
	// input that will never come.
	res, err := Run(context.Background(), []string{"cat"}, Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut {
		t.Error("cat blocked on stdin instead of seeing EOF")
	}
}

func TestRunUsesRequestedWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	result, err := Run(context.Background(), []string{"pwd"}, Options{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != resolved {
		t.Fatalf("pwd = %q, want %q", result.Stdout, resolved)
	}
}
