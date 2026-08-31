package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func join(a []string) string { return strings.Join(a, "|") }

// A rein flag typed after the intent must be honoured, not swallowed into
// the intent — that silently defeated --auto.
func TestHoistFlagsAfterIntent(t *testing.T) {
	got := hoistFlags(
		[]string{"agent-browser", "go to the first blog post", "--auto"},
		runFlags, runValueFlags,
	)
	if want := "--auto|agent-browser|go to the first blog post"; join(got) != want {
		t.Errorf("got %q, want %q", join(got), want)
	}
}

func TestHoistFlagsKeepsValueWithItsFlag(t *testing.T) {
	got := hoistFlags(
		[]string{"git", "summarise history", "--steps", "3", "--yes"},
		runFlags, runValueFlags,
	)
	if want := "--steps|3|--yes|git|summarise history"; join(got) != want {
		t.Errorf("got %q, want %q", join(got), want)
	}
}

// A dashed word that is not a rein flag belongs to the wrapped tool and must
// survive inside the intent.
func TestHoistFlagsLeavesForeignFlagsInIntent(t *testing.T) {
	args := []string{"gh", "list PRs with --json output and --force off"}
	got := hoistFlags(args, runFlags, runValueFlags)
	if join(got) != join(args) {
		t.Errorf("foreign flags were hoisted: %q", join(got))
	}
}

// "--" is the escape hatch for an intent that really does mention a rein flag.
func TestHoistFlagsTerminator(t *testing.T) {
	got := hoistFlags(
		[]string{"--yes", "--", "gh", "what does --auto do?"},
		runFlags, runValueFlags,
	)
	if want := "--yes|gh|what does --auto do?"; join(got) != want {
		t.Errorf("got %q, want %q", join(got), want)
	}
}

// With known == nil every flag is hoisted, which is what `spec` wants so that
// typos still reach the flag package as errors.
func TestHoistFlagsNilKnownHoistsEverything(t *testing.T) {
	got := hoistFlags([]string{"git", "--show", "--depth", "3"}, nil, map[string]bool{"depth": true})
	if want := "--show|--depth|3|git"; join(got) != want {
		t.Errorf("got %q, want %q", join(got), want)
	}
}

func TestHoistFlagsEqualsForm(t *testing.T) {
	got := hoistFlags([]string{"git", "do a thing", "--steps=4"}, runFlags, runValueFlags)
	if want := "--steps=4|git|do a thing"; join(got) != want {
		t.Errorf("got %q, want %q", join(got), want)
	}
}

// `rein ffmpeg "..."` is shorthand for `rein in ffmpeg "..."`: the verb is
// optional when the first positional word is an executable on PATH.
func TestImpliedTool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fakeffmpeg"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	cases := []struct {
		args []string
		want string
		ok   bool
	}{
		{[]string{"fakeffmpeg", "make a 3 second video"}, "fakeffmpeg", true},
		{[]string{"--auto", "fakeffmpeg", "make a video"}, "fakeffmpeg", true},     // flag first
		{[]string{"--steps", "3", "fakeffmpeg", "x"}, "fakeffmpeg", true},          // value flag first
		{[]string{"--steps=3", "fakeffmpeg", "x"}, "fakeffmpeg", true},             // --flag=value
		{[]string{"--", "fakeffmpeg", "what does --auto do?"}, "fakeffmpeg", true}, // terminator
		{[]string{"nosuchtool", "do a thing"}, "", false},                          // not on PATH
		{[]string{"--bogus", "fakeffmpeg", "x"}, "", false},                        // unknown flag: not ours
		{[]string{"--auto"}, "", false},                                            // no positional at all
		{[]string{}, "", false},
	}
	for _, c := range cases {
		got, ok := impliedTool(c.args)
		if got != c.want || ok != c.ok {
			t.Errorf("impliedTool(%q) = %q,%v; want %q,%v", c.args, got, ok, c.want, c.ok)
		}
	}
}
