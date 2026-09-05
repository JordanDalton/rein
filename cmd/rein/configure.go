package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jordandalton/rein/internal/spec"
)

const configureUsage = `usage: rein configure [claude-code|codex] [flags]
  claude-code|codex      without flags, start confirmed interactive project setup
  --scope project|user   project (default) or this user's Rein home
  --backend NAME         Rein's planner backend, not the outer harness backend
  --model NAME           Rein's planner model, not the outer harness model
  --require-spec TOOLS   comma-separated trusted cached specs required for setup/check
  --dry-run              preview only; never starts interactive setup
  --apply                save a Rein-owned launch profile
  --persistent           install project-level tool guard and MCP settings instead
  --register             with --apply, register the caller if missing (requires login)
  --check                check saved profile and local prerequisites; no model request
  --launch               start the harness with the saved restrictions
  --undo                 restore the previous profile, or remove a newly created one

Without --persistent, existing harness settings are never rewritten. Restrictions apply only when
starting via rein configure HOST --launch [--scope user]. Direct harness launches
are unchanged. Undo never revokes an agent identity or deletes its credentials.
`

type harnessProfile struct {
	Version int    `json:"version"`
	Host    string `json:"host"`
	Rein    string `json:"rein_binary"`
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
}

// The receipt stores exactly what was installed and the previous bytes. Undo
// refuses to clobber edits made after configuration.
type harnessReceipt struct {
	Installed []byte `json:"installed"`
	Previous  []byte `json:"previous,omitempty"`
	Existed   bool   `json:"existed"`
}

func cmdConfigure(ctx context.Context, args []string) error {
	if len(args) == 1 && args[0] == "--tool-guard" {
		return runToolGuard(os.Stdin, os.Stdout)
	}
	if len(args) == 0 {
		fmt.Print(configureUsage)
		for _, host := range []string{"claude-code", "codex"} {
			binary, err := exec.LookPath(harnessBinary(host))
			if err != nil {
				fmt.Printf("%s: not found on PATH\n", host)
			} else {
				fmt.Printf("%s: %s\n", host, binary)
			}
		}
		return nil
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Print(configureUsage)
		return nil
	}
	host := args[0]
	if host != "claude-code" && host != "codex" {
		return errors.New(configureUsage)
	}
	if len(args) == 1 {
		return guidedHarnessSetup(ctx, host)
	}
	fs := flagSet("configure")
	scope := fs.String("scope", "project", "")
	dry := fs.Bool("dry-run", false, "")
	apply := fs.Bool("apply", false, "")
	check := fs.Bool("check", false, "")
	launch := fs.Bool("launch", false, "")
	undo := fs.Bool("undo", false, "")
	register := fs.Bool("register", false, "")
	backend := fs.String("backend", "", "")
	model := fs.String("model", "", "")
	persistent := fs.Bool("persistent", false, "")
	requiredSpecs := fs.String("require-spec", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || (*scope != "project" && *scope != "user") {
		return errors.New(configureUsage)
	}
	if *requiredSpecs != "" {
		if *undo {
			return errors.New("--require-spec cannot be used with --undo")
		}
		if err := checkRequiredSpecs(ctx, *requiredSpecs); err != nil {
			return err
		}
		if len(args) == 3 && args[1] == "--require-spec" || len(args) == 2 && strings.HasPrefix(args[1], "--require-spec=") {
			return guidedHarnessSetup(ctx, host)
		}
	}
	actions := 0
	for _, enabled := range []bool{*dry, *apply, *check, *launch, *undo} {
		if enabled {
			actions++
		}
	}
	if actions > 1 || (*register && !*apply) {
		return errors.New("choose one action; --register is only available with --apply")
	}
	if (*check || *launch || *undo) && (*backend != "" || *model != "") {
		return errors.New("--backend and --model configure a saved profile; use them with preview or --apply, not --check/--launch/--undo")
	}
	if strings.ContainsAny(*backend+*model, "\x00\r\n") {
		return errors.New("backend and model must not contain control characters")
	}
	if *persistent {
		if *scope != "project" || *launch || *register {
			return errors.New("persistent setup supports project scope and preview/apply/check/undo; register the caller separately, then launch the harness normally")
		}
		if *apply || *check {
			if err := checkPersistentHost(ctx, host); err != nil {
				return err
			}
		}
		return configurePersistent(host, *backend, *model, *apply, *check, *undo)
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if *scope == "user" {
		root = spec.Home()
	} else {
		root = filepath.Join(root, ".rein")
	}
	path := filepath.Join(root, "harnesses", host+".json")
	if err := rejectHarnessSymlinks(path); err != nil {
		return err
	}
	if *undo {
		return undoHarnessProfile(path)
	}
	if *check || *launch {
		profile, err := readHarnessProfile(path, host)
		if err != nil {
			return err
		}
		if err := checkHarnessProfile(profile); err != nil {
			return err
		}
		fmt.Println(harnessCoverage(host))
		if !*launch {
			fmt.Println("Local prerequisites checked. Live MCP initialization and bypass tests are still required.")
			return nil
		}
		binary, err := exec.LookPath(harnessBinary(host))
		if err != nil {
			return err
		}
		command := exec.CommandContext(ctx, binary, harnessArgs(profile)...)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		return command.Run()
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	profile := harnessProfile{Version: 1, Host: host, Rein: binary, Backend: *backend, Model: *model}
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	fmt.Printf("Profile: %s\n%s\n%s", path, harnessCoverage(host), data)
	launchArgs, _ := json.MarshalIndent(harnessArgs(profile), "", "  ")
	fmt.Printf("Launch executable: %s\nExact argument array (passed directly, not through a shell):\n%s\n", harnessBinary(host), launchArgs)
	if _, err := exec.LookPath(harnessBinary(host)); err != nil {
		fmt.Printf("Warning: %s is not installed or not on PATH.\n", harnessBinary(host))
	}
	fmt.Println("Existing harness settings remain unchanged. Native tools unsupported by Rein must not be used as fallbacks.")
	if !*apply {
		fmt.Println("Preview only. Repeat this command with the same backend/model options and --apply instead of --dry-run to save.")
		return nil
	}
	if *register {
		agents, err := loadAgents()
		if err != nil {
			return err
		}
		found := false
		for _, agent := range agents.Agents {
			if agent.Provider == host {
				found = true
			}
		}
		if !found {
			fmt.Printf("Registering %s with the active cloud profile. Registration is not undone by --undo.\n", host)
			if err := cmdAgent(ctx, []string{"register", host}); err != nil {
				return err
			}
		}
	}
	if err := saveHarnessProfile(path, data); err != nil {
		return err
	}
	fmt.Printf("Saved. Check: rein configure %s --scope %s --check\nLaunch: rein configure %s --scope %s --launch\n", host, *scope, host, *scope)
	if _, err := registeredAgent(host); err != nil {
		fmt.Printf("Caller not ready: %v\n", err)
	}
	return nil
}

func harnessBinary(host string) string {
	if host == "claude-code" {
		return "claude"
	}
	return "codex"
}

func harnessCoverage(host string) string {
	if host == "claude-code" {
		return "STRICT LAUNCH PLAN / enforcement UNVERIFIED: no built-in tools; Rein-only MCP configuration. Audit managed MCP, plugins, hooks, and host automation separately."
	}
	return "PARTIAL COVERAGE: native shell disabled; Rein required at startup. Native patch tools, other MCP servers, plugins, apps, code runtimes, and delegated tools may remain. NOT Rein-only enforcement."
}

func harnessArgs(p harnessProfile) []string {
	mcpArgs := []string{"mcp", "--agent", p.Host}
	if p.Backend != "" {
		mcpArgs = append(mcpArgs, "--backend", p.Backend)
	}
	if p.Model != "" {
		mcpArgs = append(mcpArgs, "--model", p.Model)
	}
	if p.Host == "claude-code" {
		config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"rein": map[string]any{"command": p.Rein, "args": mcpArgs, "alwaysLoad": true}}})
		return []string{"--tools", "", "--strict-mcp-config", "--mcp-config", string(config)}
	}
	// JSON strings are valid TOML basic strings for normal filesystem paths.
	quoted, _ := json.Marshal(p.Rein)
	encodedArgs, _ := json.Marshal(mcpArgs)
	server := fmt.Sprintf("mcp_servers.rein={command=%s,args=%s,enabled=true,required=true}", quoted, encodedArgs)
	return []string{"-c", "features.shell_tool=false", "-c", server}
}

func readHarnessProfile(path, host string) (harnessProfile, error) {
	var p harnessProfile
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, err
	}
	if p.Version != 1 || p.Host != host || !filepath.IsAbs(p.Rein) || strings.ContainsAny(p.Rein, "\x00\r\n") {
		return p, errors.New("invalid harness profile; preview and apply configuration again")
	}
	if strings.ContainsAny(p.Backend+p.Model, "\x00\r\n") {
		return p, errors.New("invalid backend/model in harness profile")
	}
	return p, nil
}

func checkHarnessProfile(p harnessProfile) error {
	if _, err := exec.LookPath(harnessBinary(p.Host)); err != nil {
		return err
	}
	info, err := os.Stat(p.Rein)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("Rein binary is not a regular file")
	}
	if _, err := exec.LookPath(p.Rein); err != nil {
		return fmt.Errorf("Rein binary is not executable: %w", err)
	}
	_, err = registeredAgent(p.Host)
	return err
}

func rejectHarnessSymlinks(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in configuration path: %s", current)
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func writeHarnessExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func saveHarnessProfile(path string, data []byte) error {
	if err := rejectHarnessSymlinks(path); err != nil {
		return err
	}
	previous, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && bytes.Equal(previous, data) {
		return nil
	}
	receipt := harnessReceipt{Installed: data, Previous: previous, Existed: err == nil}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	backup, _ := json.Marshal(receipt)
	// Keep one undo point; require undo before replacing a different setup.
	if err := writeHarnessExclusive(path+".undo", backup); err != nil {
		return fmt.Errorf("cannot create backup (undo previous setup before replacing it): %w", err)
	}
	if receipt.Existed {
		return replaceHarnessFile(path, data)
	}
	return writeHarnessExclusive(path, data)
}

func undoHarnessProfile(path string) error {
	if err := rejectHarnessSymlinks(path); err != nil {
		return err
	}
	if err := rejectHarnessSymlinks(path + ".undo"); err != nil {
		return err
	}
	backup, err := os.ReadFile(path + ".undo")
	if err != nil {
		return err
	}
	var receipt harnessReceipt
	if err := json.Unmarshal(backup, &receipt); err != nil {
		return err
	}
	if len(receipt.Installed) == 0 {
		return errors.New("invalid undo receipt")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, receipt.Installed) {
		return errors.New("profile changed since configure; refusing to overwrite your edits")
	}
	if receipt.Existed {
		err = replaceHarnessFile(path, receipt.Previous)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return err
	}
	if err := os.Remove(path + ".undo"); err != nil {
		return err
	}
	fmt.Println("Rein launch profile undone. Harness settings and registered credentials were not changed.")
	return nil
}

func replaceHarnessFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".rein-configure-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
