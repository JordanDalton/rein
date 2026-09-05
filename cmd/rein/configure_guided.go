package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func confirmSetup(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Continue? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	return err == nil && (strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes"))
}

func guidedHarnessName(host string) string {
	if host == "claude-code" {
		return "Claude Code"
	}
	return "Codex"
}

func printGuidedSetupPlan(out io.Writer, host, project string) {
	fmt.Fprintf(out, "\nRein setup — %s\n\n", guidedHarnessName(host))
	fmt.Fprintf(out, "  Project     %s\n", project)
	fmt.Fprintf(out, "  Agent       %s\n", host)
	fmt.Fprintln(out, "  Connection  Persistent Rein Gateway")
	fmt.Fprintln(out, "  Approval    Read-only by default")
	fmt.Fprintln(out, "\nThis will:")
	fmt.Fprintln(out, "  • reuse or register the agent identity")
	fmt.Fprintln(out, "  • start the local Gateway")
	fmt.Fprintln(out, "  • install project guardrails and the MCP bridge")
	fmt.Fprintln(out, "  • verify the MCP handshake")
	fmt.Fprintln(out, "\nNo model requests or tool commands will run during setup.")
	if host == "codex" {
		fmt.Fprintln(out, "Codex coverage is partial; review /mcp, /hooks, and alternate tools after setup.")
	} else {
		fmt.Fprintln(out, "Trust the project settings, then review /mcp and /hooks after setup.")
	}
	fmt.Fprintln(out)
}

func printGuidedSetupComplete(out io.Writer, host string) {
	fmt.Fprintf(out, "\nReady — %s is connected through Rein Gateway.\n\n", guidedHarnessName(host))
	fmt.Fprintln(out, "  ✓ Agent identity ready")
	fmt.Fprintln(out, "  ✓ Gateway running")
	fmt.Fprintln(out, "  ✓ Project guardrails installed")
	fmt.Fprintln(out, "  ✓ MCP handshake verified")
	fmt.Fprintln(out, "\nNext:")
	fmt.Fprintf(out, "  1. Start a fresh %s session.\n", guidedHarnessName(host))
	fmt.Fprintln(out, "  2. Trust this project's settings and inspect /mcp and /hooks.")
	fmt.Fprintln(out, "  3. Ask for a harmless operation and confirm it appears in Rein Activity.")
	fmt.Fprintf(out, "\nUndo: rein undo %s\n", host)
}

func guidedHarnessProfile(host string, save bool) error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	path := filepath.Join(root, ".rein", "harnesses", host+".json")
	if err := rejectHarnessSymlinks(path); err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	profile := harnessProfile{Version: 1, Host: host, Rein: binary, Gateway: true}
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	existing, readErr := os.ReadFile(path)
	if readErr == nil && !bytes.Equal(existing, data) {
		return fmt.Errorf("existing launch profile uses a different binary or transport; run `rein undo %s`, then retry `rein configure %s`", host, host)
	}
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if !save || readErr == nil {
		return nil
	}
	return saveHarnessProfile(path, data)
}

func guidedHarnessSetup(ctx context.Context, host string) error {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("interactive setup requires a terminal; use explicit --dry-run or --persistent --apply flags")
	}
	if err := checkPersistentHost(ctx, host); err != nil {
		return fmt.Errorf("%s prerequisites not ready; install a supported version and sign in before setup: %w", host, err)
	}
	input := bufio.NewReader(os.Stdin)
	missing, err := repairPersistentMissing(host, false)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		fmt.Printf("Missing Rein-managed files: %s\nRestore these files from the installation receipt? Existing files and undo backups will not be changed.\n", strings.Join(missing, ", "))
		if !confirmSetup(input, os.Stdout) {
			fmt.Println("Cancelled. No changes made.")
			return nil
		}
		if _, err := repairPersistentMissing(host, true); err != nil {
			return err
		}
		fmt.Println("Missing files restored. Continuing setup verification.")
	}
	if err := configurePersistentQuiet(host, "", "", true, false, false, false); err != nil {
		if errors.Is(err, errPersistentSetupConflict) {
			return fmt.Errorf("existing setup uses different binary, transport, or planner options; run `rein undo %s`, then retry `rein configure %s`", host, host)
		}
		return err
	}
	if err := guidedHarnessProfile(host, false); err != nil {
		return err
	}
	project, err := os.Getwd()
	if err != nil {
		return err
	}
	printGuidedSetupPlan(os.Stdout, host, project)
	if !confirmSetup(input, os.Stdout) {
		fmt.Println("Setup cancelled. Any confirmed repair remains installed.")
		return nil
	}
	p, err := loadCloudProfile()
	if err != nil {
		return err
	}
	if p == nil {
		if err := cmdLogin(ctx, nil); err != nil {
			return err
		}
	}
	if _, err := registeredAgent(host); err != nil {
		if err := cmdAgent(ctx, []string{"register", host}); err != nil {
			return err
		}
	}
	// Fail before installing restrictions if the saved origin or agent credential is invalid.
	if _, err := newMCPGoverned(host); err != nil {
		return fmt.Errorf("connection not ready (use rein login --control-url https://reincontrol.com): %w", err)
	}
	opts, err := parseGatewayOptions("start", nil)
	if err != nil {
		return err
	}
	if err := startGatewayQuiet(ctx, opts); err != nil {
		return fmt.Errorf("gateway could not be started: %w", err)
	}
	if err := guidedHarnessProfile(host, true); err != nil {
		return fmt.Errorf("launch profile could not be saved; existing profiles are protected (use --undo before replacing a different profile): %w", err)
	}
	if err := configurePersistentQuiet(host, "", "", true, true, false, false); err != nil {
		return fmt.Errorf("persistent setup incomplete; launch profile remains saved (restore it with rein configure %s --undo): %w", host, err)
	}
	if err := configurePersistentQuiet(host, "", "", false, false, true, false); err != nil {
		return err
	}
	if err := checkHarnessMCP(ctx, host); err != nil {
		return fmt.Errorf("setup incomplete: MCP verification failed: %w. Settings and launch profile remain installed; fix the cause and rerun setup, or restore with `rein undo %s`", err, host)
	}
	printGuidedSetupComplete(os.Stdout, host)
	return nil
}

// Launch the exact installed server, without invoking models or executing tools.
func checkClaudeMCP(ctx context.Context) error {
	return checkHarnessMCP(ctx, "claude-code")
}

func checkHarnessMCP(ctx context.Context, host string) error {
	path := ".mcp.json"
	if host == "codex" {
		path = ".codex/config.toml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config struct {
		Servers map[string]struct {
			Command string   `json:"command" toml:"command"`
			Args    []string `json:"args" toml:"args"`
		} `json:"mcpServers" toml:"mcp_servers"`
	}
	if host == "codex" {
		err = toml.Unmarshal(data, &config)
	} else {
		err = json.Unmarshal(data, &config)
	}
	if err != nil {
		return err
	}
	server, ok := config.Servers["rein"]
	if !ok || server.Command == "" {
		return errors.New("missing Rein MCP configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, server.Command, server.Args...)
	command.Stderr = os.Stderr
	in, err := command.StdinPipe()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	defer func() { cancel(); _ = command.Wait() }()
	if _, err := io.WriteString(in, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"clientInfo\":{\"name\":\"rein-setup\",\"version\":\"1\"}}}\n"); err != nil {
		return err
	}
	var response struct {
		ID     int `json:"id"`
		Result struct {
			Protocol string `json:"protocolVersion"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(out, 1<<20)).Decode(&response); err != nil {
		return fmt.Errorf("server did not initialize (see startup error above): %w", err)
	}
	if response.ID != 1 || response.Result.Protocol == "" || len(response.Error) != 0 {
		return errors.New("invalid MCP initialize response")
	}
	_, err = io.WriteString(in, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
	return err
}
