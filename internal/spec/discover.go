package spec

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// probeConcurrency is how many help probes run at once during discovery.
const probeConcurrency = 8

// Options tunes the discovery crawl. Zero values get sane defaults.
type Options struct {
	MaxDepth    int           // how deep to recurse into subcommands (default 2)
	MaxCommands int           // hard cap on discovered commands (default 60)
	Timeout     time.Duration // per help invocation (default 10s)
	Verbose     bool
}

func (o *Options) withDefaults() {
	if o.MaxDepth == 0 {
		o.MaxDepth = 2
	}
	if o.MaxCommands == 0 {
		o.MaxCommands = 60
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
}

var (
	ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
	// A section header that introduces a list of subcommands. Matches Cobra
	// ("Available Commands:"), kubectl ("Basic Commands (Beginner):"), git
	// ("These are common Git commands..."), and most hand-rolled help text.
	cmdHeaderRe = regexp.MustCompile(`(?i)^\S.*commands?\b.*:\s*$`)
	// A header that ends a command section. Everything else at column zero is
	// treated as a continuation — git groups its commands under unlabelled
	// prose headings ("start a working area") that would otherwise close the
	// section after the first group.
	otherHeaderRe = regexp.MustCompile(`(?i)^(usage|synopsis|options?|flags|global flags|inherited flags|examples?|environment|arguments|description|notes?|aliases|see also|learn more)\b`)
	// An indented "name  description" entry under such a header.
	cmdEntryRe = regexp.MustCompile(`^[ \t]+([a-z][a-zA-Z0-9:._-]*)(?:[ \t]{2,}(.*))?$`)
	flagRe     = regexp.MustCompile(`(--[a-zA-Z0-9][a-zA-Z0-9-]*)`)
)

// StripANSI removes escape sequences so help text and command output stay
// legible to the model (and cheap in tokens).
func StripANSI(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\r", "")
}

// Discover builds a capability map by crawling the tool's own help output.
func Discover(ctx context.Context, tool string, opts Options) (*Spec, error) {
	opts.withDefaults()

	bin, err := exec.LookPath(tool)
	if err != nil {
		return nil, fmt.Errorf("cannot find %q on PATH: %w", tool, err)
	}

	s := &Spec{
		Tool:         tool,
		Binary:       bin,
		DiscoveredAt: time.Now(),
		Version:      detectVersion(ctx, bin, opts),
		Source:       "help",
	}

	root, err := helpFor(ctx, bin, nil, opts)
	if err != nil {
		return nil, fmt.Errorf("%s produced no usable help output: %w", tool, err)
	}
	s.RootHelp = root

	// Cobra-family tools ship a machine-readable completion endpoint. When it
	// exists it beats parsing prose, so try it first and fall back to the help
	// text parser.
	seeds := cobraComplete(ctx, bin, nil, opts)
	if len(seeds) > 0 {
		s.Source = "completion"
	} else {
		seeds = parseSubcommands(root)
	}

	type queued struct {
		path    []string
		summary string
		depth   int
	}
	var queue []queued
	for _, e := range seeds {
		queue = append(queue, queued{path: []string{e.name}, summary: e.summary, depth: 1})
	}

	// Probes are independent subprocess runs, almost entirely wait time, so
	// they go out in concurrent waves. Results are folded back in queue order,
	// which keeps the crawl deterministic and the MaxCommands cap meaningful.
	type probed struct {
		help     string
		err      error
		children []entry
	}
	seen := map[string]bool{}
	for len(queue) > 0 && len(s.Commands) < opts.MaxCommands {
		var wave []queued
		for len(wave) < probeConcurrency && len(queue) > 0 &&
			len(s.Commands)+len(wave) < opts.MaxCommands {
			q := queue[0]
			queue = queue[1:]
			key := strings.Join(q.path, " ")
			if seen[key] {
				continue
			}
			seen[key] = true
			if opts.Verbose {
				fmt.Fprintf(os.Stderr, "  probing %s %s\n", tool, key)
			}
			wave = append(wave, q)
		}

		results := make([]probed, len(wave))
		var wg sync.WaitGroup
		for i, q := range wave {
			wg.Add(1)
			go func(i int, q queued) {
				defer wg.Done()
				r := probed{}
				r.help, r.err = helpFor(ctx, bin, q.path, opts)
				if r.err == nil && q.depth < opts.MaxDepth {
					r.children = cobraComplete(ctx, bin, q.path, opts)
					if len(r.children) == 0 {
						r.children = parseSubcommands(r.help)
					}
				}
				results[i] = r
			}(i, q)
		}
		wg.Wait()

		for i, q := range wave {
			r := results[i]
			if r.err != nil {
				// A subcommand with no help is still worth recording by name.
				s.Commands = append(s.Commands, Command{Path: q.path, Summary: q.summary})
				continue
			}
			s.Commands = append(s.Commands, Command{
				Path:    q.path,
				Summary: q.summary,
				Flags:   parseFlags(r.help),
				Help:    r.help,
			})
			for _, c := range r.children {
				queue = append(queue, queued{
					path:    append(append([]string{}, q.path...), c.name),
					summary: c.summary,
					depth:   q.depth + 1,
				})
			}
		}
	}

	sort.Slice(s.Commands, func(i, j int) bool { return s.Commands[i].Name() < s.Commands[j].Name() })
	return s, nil
}

// run executes the binary with a timeout and returns combined output. Help
// text routinely goes to stderr, so the streams are merged deliberately.
func run(ctx context.Context, bin string, args []string, opts Options) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(),
		"TERM=dumb", "NO_COLOR=1", "CLICOLOR=0", "CLICOLOR_FORCE=0",
		"PAGER=cat", "GIT_PAGER=cat", "AWS_PAGER=", "GH_PAGER=cat",
		"COLUMNS=100",
	)
	cmd.Stdin = bytes.NewReader(nil) // never let a probe block on input

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return StripANSI(buf.String()), err
}

// helpFor tries the usual help incantations in order of prevalence.
func helpFor(ctx context.Context, bin string, path []string, opts Options) (string, error) {
	for _, probe := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		args := append(append([]string{}, path...), probe...)
		out, _ := run(ctx, bin, args, opts)
		// Exit status is unreliable here: plenty of tools exit non-zero after
		// printing perfectly good usage. Judge by the output instead.
		if looksLikeHelp(out) {
			return strings.TrimSpace(out), nil
		}
	}
	return "", fmt.Errorf("no help output")
}

func looksLikeHelp(s string) bool {
	if len(strings.TrimSpace(s)) < 40 {
		return false
	}
	l := strings.ToLower(s)
	return strings.Contains(l, "usage") || strings.Contains(l, "options") ||
		strings.Contains(l, "commands") || strings.Contains(l, "--help")
}

func detectVersion(ctx context.Context, bin string, opts Options) string {
	for _, probe := range [][]string{{"--version"}, {"version"}, {"-V"}, {"-v"}} {
		out, err := run(ctx, bin, probe, opts)
		if err != nil {
			continue
		}
		line := strings.TrimSpace(firstLine(out))
		if line != "" && len(line) < 200 {
			return line
		}
	}
	return ""
}

type entry struct{ name, summary string }

// cobraComplete asks a Cobra-based CLI for its own subcommand list. Output is
// "name\tdescription" lines terminated by a ":<directive>" line.
func cobraComplete(ctx context.Context, bin string, path []string, opts Options) []entry {
	args := append([]string{"__complete"}, path...)
	args = append(args, "")
	out, err := run(ctx, bin, args, opts)
	if err != nil {
		return nil
	}
	var res []entry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			break // directive line ends the candidate list
		}
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "Completion ended") {
			continue
		}
		name, summary, _ := strings.Cut(line, "\t")
		name = strings.TrimSpace(name)
		if !isCommandName(name) {
			continue
		}
		res = append(res, entry{name: name, summary: strings.TrimSpace(summary)})
	}
	return res
}

// parseSubcommands extracts a subcommand list from prose help text.
func parseSubcommands(help string) []entry {
	var res []entry
	seen := map[string]bool{}
	inSection := false

	for _, line := range strings.Split(help, "\n") {
		trimmed := strings.TrimSpace(line)
		indented := line != "" && (line[0] == ' ' || line[0] == '\t')

		if !indented && trimmed != "" {
			switch {
			case cmdHeaderRe.MatchString(line):
				inSection = true
			case otherHeaderRe.MatchString(line):
				inSection = false
			}
			continue
		}
		if !inSection || trimmed == "" {
			continue
		}
		m := cmdEntryRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if !isCommandName(name) || seen[name] {
			continue
		}
		seen[name] = true
		res = append(res, entry{name: name, summary: strings.TrimSpace(m[2])})
	}
	return res
}

var notCommands = map[string]bool{
	"usage": true, "options": true, "flags": true, "examples": true,
	"arguments": true, "commands": true, "see": true, "note": true,
	"where": true, "for": true, "the": true, "and": true, "or": true,
	"use": true,
}

func isCommandName(s string) bool {
	if s == "" || len(s) > 40 || notCommands[s] {
		return false
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "_") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') &&
			r != '-' && r != '_' && r != ':' && r != '.' {
			return false
		}
	}
	return true
}

func parseFlags(help string) []string {
	seen := map[string]bool{}
	var res []string
	for _, m := range flagRe.FindAllStringSubmatch(help, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			res = append(res, m[1])
		}
	}
	if len(res) > 40 {
		res = res[:40]
	}
	return res
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
