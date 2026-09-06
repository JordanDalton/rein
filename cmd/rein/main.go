// Command rein wraps an arbitrary CLI in an agentic loop.
//
//	rein spec kubectl              # learn the tool, cache the capability map
//	rein kubectl "which pods are crashlooping in staging?"   # "in" is optional
//
// The design bet is that the hard part of wrapping a CLI is not the loop but
// the capability map: discovery runs once per tool version and is reused on
// every later invocation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jordandalton/rein/internal/loop"
	"github.com/jordandalton/rein/internal/planner"
	"github.com/jordandalton/rein/internal/spec"
)

const banner = `
  ____  _____ ___ _   _
 |  _ \| ____|_ _| \ | |
 | |_) |  _|  | ||  \| |
 |  _ <| |___ | || |\  |
 |_| \_\_____|___|_| \_|

 CLI execution. Governed by you.
 reincontrol.com
`

const usage = banner + `
rein — wrap a CLI in an agentic loop

usage:
  rein in <tool> <intent...>   drive <tool> until the intent is satisfied
                               (rein flags may go before or after the
                               intent; other dashed words stay in it, and
                               "--" ends flag parsing entirely)
  rein <tool> <intent...>      the same; "in" is optional when <tool> is on PATH
  rein spec <tool>             discover and cache <tool>'s capability map
  rein list                    show cached capability maps
  rein login                   connect this installation to Rein Control
                               use --remote for an SSH callback tunnel
  rein logout                  revoke this device's Rein Control credential
  rein status                  show this installation's Rein Control identity
  rein team list                list local team profiles
  rein team use NAME             switch the active team profile
  rein sync                    download the latest organization policy bundle
  rein approval list            list approval requests across agents
  rein agent register NAME     register an explicit MCP caller (codex, claude-code)
  rein agent list              list registered callers
  rein agent revoke NAME       revoke a registered caller
  rein configure [HOST]        preview harness launch or --persistent project setup
  rein undo HOST               restore the complete guided setup for a project
  rein gateway start|status    run one persistent local MCP gateway for agents
  rein ci check|run            non-interactive workload authentication and CI execution
  rein mcp                     serve rein's tools over MCP on stdio, so an
                               agent (Claude Code, Codex, …) can call it
  rein completion <shell>      print tab-completion script (bash, zsh, fish)
  rein update                  self-update to the latest release (go install)
  rein version                 print the installed version

in flags:
  --steps N        max planning steps (default 8)
  --yes            auto-approve mutating commands (destructive ones still ask)
  --auto           auto-approve everything, including destructive commands
  --dry-run        print the first proposed command and stop
  --backend NAME   claude-cli (default), api, codex-cli, grok-cli, or a
                   hosted/local OpenAI-compatible preset: openai, openrouter,
                   xai, groq, deepseek, mistral, together, ollama, lmstudio,
                   openai-compatible
  --model NAME     model for the chosen backend (required for the presets)
  --base-url URL   override the endpoint (any OpenAI-compatible API)
  --api-key-env V  read the credential from environment variable V
  --timeout D      per-command timeout (default 60s)
  --refresh        rediscover the capability map before running
  -v               show the planner's reasoning

mcp flags:
  --yes / --auto   the most a caller may be granted; a request for more is
                   refused (default: read-only commands only)
  --steps, --timeout, --backend, --model, --base-url, --api-key-env
                   as for "in"; they apply to every call

spec flags:
  --show           print the cached map instead of rediscovering
  --depth N        subcommand recursion depth (default 2)
  --max N          max commands to discover (default 60)
  -v               log each probe

examples:
  rein spec gh
  rein in gh "how many open PRs are assigned to me?"
  rein git "what changed in the last three commits?"
  rein in --backend ollama --model qwen2.5 git "summarise recent work"
  rein in --backend openai --base-url https://api.groq.com/openai/v1 \
          --model llama-3.3-70b-versatile gh "list my repos"
  claude mcp add rein -- rein mcp --yes
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, loop.ErrDenied) {
			fmt.Fprintln(os.Stderr, "\nstopped.")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "\nrein: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "in", "run": // "run" is an undocumented alias: muscle memory from every other agent CLI
		return cmdRun(ctx, args[1:])
	case "spec":
		return cmdSpec(ctx, args[1:])
	case "list":
		return cmdList()
	case "login":
		return cmdLogin(ctx, args[1:])
	case "status":
		return cmdStatus(ctx, args[1:])
	case "team":
		return cmdTeam(args[1:])
	case "sync":
		return cmdSync(ctx, args[1:])
	case "approval":
		return cmdApproval(ctx, args[1:])
	case "logout":
		return cmdLogout(ctx, args[1:])
	case "agent":
		return cmdAgent(ctx, args[1:])
	case "configure":
		return cmdConfigure(ctx, args[1:])
	case "undo":
		return cmdUndo(args[1:])
	case "gateway":
		return cmdGateway(ctx, args[1:])
	case "ci":
		return cmdCI(ctx, args[1:])
	case "mcp":
		return cmdMCP(ctx, args[1:])
	case "completion":
		return cmdCompletion(args[1:])
	case "update":
		return cmdUpdate(ctx)
	case "version", "--version":
		return cmdVersion()
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	default:
		// `rein ffmpeg "..."` is `rein in ffmpeg "..."`: the verb is optional
		// when the first positional word is a tool on PATH. Rein's own
		// commands always win, so a tool that happens to be called `list`
		// needs the explicit `rein in list "..."`.
		if _, ok := impliedTool(args); ok {
			return cmdRun(ctx, args)
		}
		return fmt.Errorf("unknown command %q: not a rein command, and no such tool on PATH (try `rein help`)", args[0])
	}
}

// impliedTool finds the first positional word in args, skipping rein's own
// `in` flags and their values, and reports it if it resolves to an executable.
func impliedTool(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				if _, err := exec.LookPath(args[i+1]); err == nil {
					return args[i+1], true
				}
			}
			return "", false
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			name, _, hasEq := strings.Cut(strings.TrimLeft(a, "-"), "=")
			if !runFlags[name] {
				return "", false
			}
			if !hasEq && runValueFlags[name] {
				i++
			}
			continue
		}
		if _, err := exec.LookPath(a); err != nil {
			return "", false
		}
		return a, true
	}
	return "", false
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("in", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(usage) }
	steps := fs.Int("steps", 8, "")
	yes := fs.Bool("yes", false, "")
	auto := fs.Bool("auto", false, "")
	dry := fs.Bool("dry-run", false, "")
	backend := fs.String("backend", "claude-cli", "")
	model := fs.String("model", "", "")
	baseURL := fs.String("base-url", "", "")
	keyEnv := fs.String("api-key-env", "", "")
	timeout := fs.Duration("timeout", 60*time.Second, "")
	refresh := fs.Bool("refresh", false, "")
	verbose := fs.Bool("v", false, "")
	if err := fs.Parse(hoistFlags(args, runFlags, runValueFlags)); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 2 {
		return errors.New("usage: rein in <tool> <intent...>")
	}
	tool, intent := rest[0], strings.Join(rest[1:], " ")

	sp, err := ensureSpec(ctx, tool, *refresh, spec.Options{Verbose: *verbose})
	if err != nil {
		return err
	}

	be, err := makeBackend(*backend, *model, *baseURL, *keyEnv)
	if err != nil {
		return err
	}
	// Session-holding backends keep a child process alive between steps.
	if closer, ok := be.(io.Closer); ok {
		defer closer.Close()
	}

	approval := loop.ApproveSafe
	if *yes {
		approval = loop.ApproveCaution
	}
	if *auto {
		approval = loop.ApproveAll
		fmt.Fprintln(os.Stderr, "\033[33mwarning: --auto approves destructive commands without asking\033[0m")
	}

	fmt.Printf("\033[1m%s\033[0m · %s · %s\n", tool, describeVersion(sp), be.Name())
	fmt.Printf("\033[2mintent: %s\033[0m\n", intent)

	answer, err := loop.Run(ctx, loop.Config{
		Spec:     sp,
		Backend:  be,
		Intent:   intent,
		MaxSteps: *steps,
		Approval: approval,
		DryRun:   *dry,
		Timeout:  *timeout,
		Verbose:  *verbose,
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n%s\n", answer)
	return nil
}

func cmdSpec(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("spec", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(usage) }
	show := fs.Bool("show", false, "")
	depth := fs.Int("depth", 2, "")
	max := fs.Int("max", 60, "")
	verbose := fs.Bool("v", false, "")
	if err := fs.Parse(hoistFlags(args, nil, map[string]bool{"depth": true, "max": true})); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: rein spec <tool> [--show] [--depth N] [--max N]")
	}
	tool := fs.Arg(0)

	if *show {
		sp, err := spec.Load(tool)
		if err != nil {
			return err
		}
		if sp == nil {
			return fmt.Errorf("no cached spec for %q — run `rein spec %s` first", tool, tool)
		}
		fmt.Println(sp.Digest(1 << 20))
		return nil
	}

	fmt.Printf("discovering %s…\n", tool)
	start := time.Now()
	sp, err := spec.Discover(ctx, tool, spec.Options{MaxDepth: *depth, MaxCommands: *max, Verbose: *verbose})
	if err != nil {
		return err
	}
	if err := sp.Save(); err != nil {
		return err
	}
	fmt.Printf("learned %d commands from %s in %s (source: %s)\n",
		len(sp.Commands), describeVersion(sp), time.Since(start).Round(time.Millisecond), sp.Source)
	fmt.Printf("cached at %s\n", sp.Path())
	return nil
}

func cmdList() error {
	specs, err := cachedSpecs()
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		fmt.Println("no cached specs yet — try `rein spec git`")
		return nil
	}
	for _, sp := range specs {
		fmt.Println(describeSpec(sp))
	}
	return nil
}

// cachedSpecs loads every capability map in the cache, sorted by tool name.
// Unreadable entries are skipped: one corrupt file should not hide the rest.
func cachedSpecs() ([]*spec.Spec, error) {
	dir := filepath.Join(spec.Home(), "specs")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	var specs []*spec.Spec
	for _, n := range names {
		sp, err := spec.Load(n)
		if err != nil || sp == nil {
			continue
		}
		specs = append(specs, sp)
	}
	return specs, nil
}

func describeSpec(sp *spec.Spec) string {
	return fmt.Sprintf("%-14s %3d commands  %s  %s", sp.Tool, len(sp.Commands),
		sp.DiscoveredAt.Format("2006-01-02"), sp.Version)
}

// ensureSpec loads the cached capability map, discovering one on first use.
func ensureSpec(ctx context.Context, tool string, refresh bool, opts spec.Options) (*spec.Spec, error) {
	if !refresh {
		sp, err := spec.Load(tool)
		if err != nil {
			return nil, err
		}
		if sp != nil {
			return sp, nil
		}
	}
	fmt.Fprintf(os.Stderr, "\033[2mno capability map for %s yet — discovering (one time)…\033[0m\n", tool)
	sp, err := spec.Discover(ctx, tool, opts)
	if err != nil {
		return nil, err
	}
	if err := sp.Save(); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "\033[2mlearned %d commands\033[0m\n", len(sp.Commands))
	return sp, nil
}

// hoistFlags reorders args so flags may appear on either side of the
// positional arguments. Go's flag package stops at the first non-flag token,
// which would otherwise make `rein spec git --show` silently drop the flag.
//
// When known is non-nil, only flags in that set are hoisted and everything else
// is left in place — `rein in gh "PRs with --json output"` must keep the
// --json inside the intent. A "--" terminator ends hoisting entirely, which is
// the escape hatch for an intent that really does mention a rein flag.
func hoistFlags(args []string, known, takesValue map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		name, _, hasEq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if known != nil && !known[name] {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if !hasEq && takesValue[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// runFlags is rein's own flag set for `in`. Only these are hoisted out of
// an intent; any other dashed word belongs to the wrapped tool.
var runFlags = map[string]bool{
	"steps": true, "yes": true, "auto": true, "dry-run": true,
	"backend": true, "model": true, "timeout": true, "refresh": true, "v": true,
	"base-url": true, "api-key-env": true,
}

var runValueFlags = map[string]bool{
	"steps": true, "backend": true, "model": true, "timeout": true, "base-url": true, "api-key-env": true,
}

func makeBackend(name, model, baseURL, keyEnv string) (planner.Backend, error) {
	switch name {
	case "claude-cli", "cli", "":
		return &planner.ClaudeCLI{Model: model}, nil
	case "codex-cli", "codex":
		return &planner.CodexCLI{Model: model}, nil
	case "grok-cli":
		return &planner.GrokCLI{Model: model}, nil
	case "api":
		return planner.NewAPI(model), nil
	default:
		// Everything else speaks the OpenAI chat-completions wire format.
		return planner.NewOpenAI(name, baseURL, model, keyEnv)
	}
}

func describeVersion(s *spec.Spec) string {
	if s.Version == "" {
		return s.Tool
	}
	return s.Version
}
