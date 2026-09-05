package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func checkPersistentHost(ctx context.Context, host string) error {
	binary, err := exec.LookPath(harnessBinary(host))
	if err != nil {
		return err
	}
	if host != "codex" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "features", "list").Output()
	if err != nil {
		return fmt.Errorf("cannot inspect Codex hook support: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "hooks" && fields[1] != "removed" {
			return nil
		}
	}
	return errors.New("installed Codex does not report hook support; upgrade before persistent setup (no settings changed)")
}

// Never return "allow": Rein's own policy and the host's other deny rules still apply.
func runToolGuard(input io.Reader, output io.Writer) error {
	var event struct {
		Event string `json:"hook_event_name"`
		Tool  string `json:"tool_name"`
	}
	data, err := io.ReadAll(io.LimitReader(input, 1024*1024+1))
	valid := err == nil && len(data) <= 1024*1024 && json.Unmarshal(data, &event) == nil && event.Event == "PreToolUse"
	if valid {
		switch event.Tool {
		case "mcp__rein__rein_in", "mcp__rein__rein_list", "mcp__rein__rein_spec":
			_, err = io.WriteString(output, "{}\n")
			return err
		}
	}
	return json.NewEncoder(output).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": "This project routes operations through Rein MCP. Use Rein; do not fall back to native tools, other servers, or delegated agents.",
		},
	})
}

type persistentFile struct {
	Path    string
	Before  []byte
	After   []byte
	Existed bool
	Mode    uint32
}
type persistentReceipt struct {
	Version int
	Host    string
	Binary  string
	Files   []persistentFile
}

func persistentPaths(host string) []string {
	if host == "claude-code" {
		return []string{".claude/settings.json", ".mcp.json"}
	}
	return []string{".codex/config.toml"}
}

func persistentObject(parent map[string]any, key string) (map[string]any, error) {
	if value, exists := parent[key]; exists {
		object, ok := value.(map[string]any)
		if !ok || object == nil {
			return nil, fmt.Errorf("%s must be an object/table", key)
		}
		return object, nil
	}
	object := map[string]any{}
	parent[key] = object
	return object, nil
}

func mergePersistent(data []byte, path string, p harnessProfile) ([]byte, error) {
	document := map[string]any{}
	if len(data) > 0 {
		var err error
		if strings.HasSuffix(path, ".toml") {
			err = toml.Unmarshal(data, &document)
		} else {
			err = json.Unmarshal(data, &document)
		}
		if err != nil || document == nil {
			return nil, fmt.Errorf("invalid configuration: %s", path)
		}
	}
	args := []string{"mcp", "--agent", p.Host}
	if p.Backend != "" {
		args = append(args, "--backend", p.Backend)
	}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	server := map[string]any{"command": p.Rein, "args": args}
	if p.Host == "claude-code" {
		// The default-deny guard intentionally blocks ToolSearch. Rein's small
		// tool set must be available without first invoking native discovery.
		server["alwaysLoad"] = true
	}
	if path != ".claude/settings.json" {
		key := "mcpServers"
		if p.Host == "codex" {
			key = "mcp_servers"
			server["enabled"] = true
			server["required"] = true
		}
		servers, err := persistentObject(document, key)
		if err != nil {
			return nil, err
		}
		// Explicitly replace only Rein's registration. All other servers remain configured,
		// but their intercepted tool calls are denied by the hook.
		servers["rein"] = server
	}
	if path != ".mcp.json" {
		if disabled, exists := document["disableAllHooks"]; exists && disabled != false {
			return nil, errors.New("hooks are disabled; resolve disableAllHooks before installing the guard")
		}
		hooks, err := persistentObject(document, "hooks")
		if err != nil {
			return nil, err
		}
		command := "'" + strings.ReplaceAll(p.Rein, "'", "'\\''") + "' configure --tool-guard"
		// Host hook launch failures otherwise may fail open. Convert a missing/crashed
		// Rein process into the host's blocking exit code, without interpolating input.
		command += " || exit 2"
		handler := map[string]any{"matcher": ".*", "hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 10}}}
		current := []any{}
		if existing, ok := hooks["PreToolUse"]; ok {
			// JSON and TOML decoders both decode array-of-tables into []any.
			encoded, _ := json.Marshal(existing)
			if err := json.Unmarshal(encoded, &current); err != nil || current == nil {
				return nil, errors.New("PreToolUse must be an array")
			}
		}
		hooks["PreToolUse"] = append(current, handler)
		if p.Host == "codex" {
			features, err := persistentObject(document, "features")
			if err != nil {
				return nil, err
			}
			features["shell_tool"] = false
			features["hooks"] = true
			document["web_search"] = "disabled"
		} else {
			permissions, err := persistentObject(document, "permissions")
			if err != nil {
				return nil, err
			}
			var deny []string
			if existing, ok := permissions["deny"]; ok {
				encoded, _ := json.Marshal(existing)
				if err := json.Unmarshal(encoded, &deny); err != nil {
					return nil, errors.New("permissions.deny must be a string array")
				}
			}
			for _, name := range []string{"Bash", "Read", "Write", "Edit", "MultiEdit", "Glob", "Grep", "NotebookEdit", "WebFetch", "WebSearch", "Agent", "Task", "Skill"} {
				found := false
				for _, item := range deny {
					if item == name {
						found = true
					}
				}
				if !found {
					deny = append(deny, name)
				}
			}
			permissions["deny"] = deny
		}
	}
	if strings.HasSuffix(path, ".toml") {
		return toml.Marshal(document)
	}
	result, err := json.MarshalIndent(document, "", "  ")
	return append(result, '\n'), err
}

func readPersistentFile(path string) (persistentFile, error) {
	f := persistentFile{Path: path, Mode: 0600}
	if err := rejectHarnessSymlinks(path); err != nil {
		return f, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if !info.Mode().IsRegular() {
		return f, fmt.Errorf("not a regular file: %s", path)
	}
	f.Existed = true
	f.Mode = uint32(info.Mode().Perm())
	f.Before, err = os.ReadFile(path)
	return f, err
}

func validatePersistentReceipt(receipt persistentReceipt, host string) error {
	paths := persistentPaths(host)
	if receipt.Version != 1 || receipt.Host != host || len(receipt.Files) != len(paths) || !filepath.IsAbs(receipt.Binary) {
		return errors.New("invalid persistent configuration receipt")
	}
	for i, f := range receipt.Files {
		if f.Path != paths[i] || len(f.After) == 0 || f.Mode > 0777 {
			return errors.New("invalid persistent configuration target")
		}
	}
	return nil
}

func configurePersistent(host, backend, model string, apply, check, undo bool) error {
	if runtime.GOOS == "windows" {
		return errors.New("persistent hooks currently support macOS/Linux only; Windows enforcement is not implemented")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// Project scope is exact: never discover or mutate parent repositories.
	receiptPath := filepath.Join(cwd, ".rein", "harnesses", host+".persistent.json")
	if err := rejectHarnessSymlinks(receiptPath); err != nil {
		return err
	}
	if saved, readErr := os.ReadFile(receiptPath); readErr == nil {
		var receipt persistentReceipt
		if json.Unmarshal(saved, &receipt) != nil {
			return errors.New("invalid persistent receipt")
		}
		if err := validatePersistentReceipt(receipt, host); err != nil {
			return err
		}
		for _, f := range receipt.Files {
			current, err := readPersistentFile(filepath.Join(cwd, f.Path))
			if err != nil {
				return err
			}
			matches := current.Existed && bytes.Equal(current.Before, f.After)
			restored := current.Existed == f.Existed && bytes.Equal(current.Before, f.Before)
			if !matches && !(undo && restored) {
				return fmt.Errorf("configuration drift in %s; refusing to overwrite edits", f.Path)
			}
		}
		if undo {
			for _, f := range receipt.Files {
				path := filepath.Join(cwd, f.Path)
				if f.Existed {
					if err := replaceHarnessFile(path, f.Before); err != nil {
						return err
					}
					if err := os.Chmod(path, os.FileMode(f.Mode)); err != nil {
						return err
					}
				} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			if err := os.Remove(receiptPath); err != nil {
				return err
			}
			fmt.Println("Persistent settings restored. Original bytes and permissions recovered; credentials unchanged.")
			return nil
		}
		if !check && (backend != "" || model != "") {
			return errors.New("undo the existing persistent setup before changing planner options")
		}
		if check {
			info, err := os.Stat(receipt.Binary)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
				return errors.New("saved Rein binary missing or not executable")
			}
		}
		fmt.Println("Persistent files match the installation receipt. Runtime enforcement is UNVERIFIED.")
		printPersistentCoverage(host)
		return nil
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if check || undo {
		return errors.New("no persistent setup found in this directory")
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	if strings.ContainsAny(binary, "\x00\r\n") {
		return errors.New("unsupported binary path")
	}
	receipt := persistentReceipt{Version: 1, Host: host, Binary: binary}
	p := harnessProfile{Host: host, Rein: binary, Backend: backend, Model: model}
	for _, relative := range persistentPaths(host) {
		f, err := readPersistentFile(filepath.Join(cwd, relative))
		if err != nil {
			return err
		}
		f.Path = relative
		f.After, err = mergePersistent(f.Before, relative, p)
		if err != nil {
			return err
		}
		receipt.Files = append(receipt.Files, f)
		fmt.Printf("Will merge %s (original backed up; TOML/JSON formatting may change).\n", relative)
	}
	fmt.Println("Plan: register Rein MCP; install a default-deny PreToolUse guard allowing only Rein's three tool names. Existing hooks and other settings are preserved and require separate audit.")
	printPersistentCoverage(host)
	if !apply {
		fmt.Println("Preview only. Repeat with --persistent --apply to install.")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0700); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(receipt, "", "  ")
	if err := writeHarnessExclusive(receiptPath, encoded); err != nil {
		return err
	}
	installed := 0
	for _, f := range receipt.Files {
		path := filepath.Join(cwd, f.Path)
		current, readErr := readPersistentFile(path)
		if readErr != nil {
			err = readErr
		} else if current.Existed != f.Existed || !bytes.Equal(current.Before, f.Before) {
			err = fmt.Errorf("configuration changed during setup: %s", f.Path)
		} else {
			err = os.MkdirAll(filepath.Dir(path), 0700)
			if err == nil {
				if f.Existed {
					err = replaceHarnessFile(path, f.After)
				} else {
					err = writeHarnessExclusive(path, f.After)
				}
			}
		}
		if err != nil {
			// Roll back only files this transaction installed, and never clobber later edits.
			for _, previous := range receipt.Files[:installed] {
				target := filepath.Join(cwd, previous.Path)
				actual, readErr := readPersistentFile(target)
				if readErr != nil || !bytes.Equal(actual.Before, previous.After) {
					continue
				}
				if previous.Existed {
					if replaceHarnessFile(target, previous.Before) == nil {
						_ = os.Chmod(target, os.FileMode(previous.Mode))
					}
				} else {
					_ = os.Remove(target)
				}
			}
			return fmt.Errorf("installation incomplete; backup retained at %s: %w", receiptPath, err)
		}
		installed++
	}
	fmt.Printf("Installed. Restart %s in this project, review/trust hooks and Rein MCP, then test bypass attempts. Check: rein configure %s --persistent --check\nUndo: rein configure %s --persistent --undo\n", harnessBinary(host), host, host)
	return nil
}

func printPersistentCoverage(host string) {
	if host == "codex" {
		fmt.Println("PARTIAL COVERAGE: requires a Codex version with PreToolUse hooks enabled and trusted via /hooks. Hosted/specialized tools and pre-existing exec sessions are not fully covered. This is NOT universal Rein-only enforcement.")
	} else {
		fmt.Println("CLAUDE GUARD INSTALLED/PLANNED, NOT RUNTIME VERIFIED: deny native tools and intercept non-Rein calls. Trust project settings/MCP and verify with /hooks. Audit plugins, other hooks, managed settings, and tool discovery separately.")
	}
	fmt.Println("User-editable project guardrails are not a security boundary. Keep protected credentials/resources available only to Rein in an administrator-controlled runtime for stronger isolation.")
}
