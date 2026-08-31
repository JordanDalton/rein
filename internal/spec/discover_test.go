package spec

import "testing"

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
