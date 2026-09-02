package spec

import (
	"strings"
	"testing"
)

const cobraHelp = `A tool for things.

Usage:
  acme [command]

Available Commands:
  apply       Apply a manifest
  get         Display one or many resources
  completion  Generate the autocompletion script

Flags:
  -h, --help      help for acme
      --verbose   chatty output
`

const gitStyleHelp = `usage: git [-v | --version] <command> [<args>]

These are common Git commands used in various situations:

start a working area
   clone      Clone a repository into a new directory
   init       Create an empty Git repository

work on the current change
   add        Add file contents to the index
   rm         Remove files from the working tree

'git help -a' lists available subcommands.
`

func names(es []entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.name)
	}
	return out
}

func TestParseSubcommandsCobra(t *testing.T) {
	got := names(parseSubcommands(cobraHelp))
	want := []string{"apply", "get", "completion"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestParseSubcommandsSurvivesUnlabelledGroups(t *testing.T) {
	// git splits its command list across prose subheadings; the parser must not
	// close the section at the first one.
	got := names(parseSubcommands(gitStyleHelp))
	if len(got) != 4 {
		t.Fatalf("got %v, want 4 commands", got)
	}
	if got[3] != "rm" {
		t.Errorf("commands after the second group heading were dropped: %v", got)
	}
}

func TestParseSubcommandsIgnoresFlagSections(t *testing.T) {
	for _, n := range names(parseSubcommands(cobraHelp)) {
		if n == "help" || n == "verbose" {
			t.Errorf("flag %q was parsed as a command", n)
		}
	}
}

func TestStripANSI(t *testing.T) {
	if got := StripANSI("\x1b[1;31mbold red\x1b[0m\r\n"); got != "bold red\n" {
		t.Errorf("got %q", got)
	}
	// nroff bold ("x\bx") and underline ("_\bx"), as man emits them.
	if got := StripANSI("g\bgi\bit\bt a\bad\bdd\bd [-\b--\b-n\bn] _\b<_\bf_\bi_\bl_\be_\b>"); got != "git add [--n] <file>" {
		t.Errorf("got %q", got)
	}
}

func TestParseFlagsSeesThroughOverstrikes(t *testing.T) {
	help := StripANSI("       -\b-n\bn, -\b--\b-d\bdr\bry\by-\b-r\bru\bun\bn\n           Don't add the file(s).\n")
	if got := parseFlags(help); len(got) != 1 || got[0] != "--dry-run" {
		t.Errorf("got %v", got)
	}
}

func TestParseFlagsExpandsNegatable(t *testing.T) {
	got := parseFlags("    -n, --[no-]dry-run    dry run\n    -f, --force\n")
	want := []string{"--dry-run", "--no-dry-run", "--force"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLooksLikeManPage(t *testing.T) {
	man := "GIT-ADD(1)                        Git Manual                        GIT-ADD(1)\n\nNAME\n       git-add - Add file contents to the index\n"
	if !looksLikeManPage(man) {
		t.Error("man page not recognised")
	}
	if looksLikeManPage("usage: git add [<options>] [--] <pathspec>...\n\n    -n, --dry-run   dry run\n") {
		t.Error("plain usage mistaken for a man page")
	}
}

func TestDigestStaysUnderBudget(t *testing.T) {
	s := &Spec{Tool: "acme", RootHelp: cobraHelp}
	for i := 0; i < 200; i++ {
		s.Commands = append(s.Commands, Command{
			Path: []string{"acme", "cmd"}, Summary: "does a thing", Help: cobraHelp,
		})
	}
	if got := len(s.Digest(8000)); got > 12000 {
		t.Errorf("digest was %d bytes, well over the 8000 budget", got)
	}
}
