// Package spec models and caches the capability map of a wrapped CLI.
//
// The capability map is the whole point of the rein: a model does not know
// your internal `acme-deploy` binary, but it can be taught one in a single
// discovery pass whose result is cached and reused on every later run.
package spec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Command is one invocable (sub)command of the wrapped CLI, flattened out of
// the help tree. Path is the argv prefix, e.g. ["kubectl", "get"].
type Command struct {
	Path    []string `json:"path"`
	Summary string   `json:"summary,omitempty"`
	Flags   []string `json:"flags,omitempty"`
	Help    string   `json:"help,omitempty"`
}

// Name renders the command as it would be typed, minus arguments.
func (c Command) Name() string { return strings.Join(c.Path, " ") }

// Spec is the cached capability map for one binary at one version.
type Spec struct {
	Tool         string    `json:"tool"`
	Binary       string    `json:"binary"`
	Version      string    `json:"version"`
	DiscoveredAt time.Time `json:"discovered_at"`
	Source       string    `json:"source"` // "completion" or "help"
	RootHelp     string    `json:"root_help"`
	Commands     []Command `json:"commands"`
}

// Home is rein's state directory. Override with REIN_HOME.
func Home() string {
	if h := os.Getenv("REIN_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".rein"
	}
	return filepath.Join(home, ".rein")
}

func cachePath(tool string) string {
	return filepath.Join(Home(), "specs", filepath.Base(tool)+".json")
}

// Load reads a cached spec. It returns nil (no error) when none exists.
func Load(tool string) (*Spec, error) {
	b, err := os.ReadFile(cachePath(tool))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("cached spec for %q is corrupt: %w", tool, err)
	}
	return &s, nil
}

// Save writes the spec to the cache.
func (s *Spec) Save() error {
	p := cachePath(s.Tool)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// Path returns where this spec is (or would be) cached.
func (s *Spec) Path() string { return cachePath(s.Tool) }

// Digest renders the spec as prompt context, staying under maxBytes. Root help
// and the per-command summaries come first because they are the highest
// signal-per-byte; per-command help text fills whatever budget is left.
func (s *Spec) Digest(maxBytes int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TOOL: %s\n", s.Tool)
	if s.Version != "" {
		fmt.Fprintf(&b, "VERSION: %s\n", s.Version)
	}
	fmt.Fprintf(&b, "\n=== %s --help ===\n%s\n", s.Tool, clip(s.RootHelp, 3000))

	cmds := append([]Command(nil), s.Commands...)
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name() < cmds[j].Name() })

	if len(cmds) > 0 {
		b.WriteString("\n=== SUBCOMMANDS ===\n")
		for _, c := range cmds {
			if c.Summary != "" {
				fmt.Fprintf(&b, "%s — %s\n", c.Name(), c.Summary)
			} else {
				fmt.Fprintf(&b, "%s\n", c.Name())
			}
		}
	}

	// Spend the remaining budget on detailed help, widest commands first.
	for _, c := range cmds {
		if c.Help == "" {
			continue
		}
		chunk := fmt.Sprintf("\n=== %s --help ===\n%s\n", c.Name(), clip(c.Help, 1200))
		if b.Len()+len(chunk) > maxBytes {
			b.WriteString("\n(remaining subcommand help omitted for length)\n")
			break
		}
		b.WriteString(chunk)
	}
	return b.String()
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}
