package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func confirmSetup(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Continue? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	return err == nil && (strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes"))
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
	if err := configurePersistent(host, "", "", false, false, false); err != nil {
		return err
	}
	fmt.Printf("Setup will use your active Rein connection (or open login), register %s if needed, install the settings shown above, and test MCP connectivity. Registration/login are not undone by --undo. No model requests or tool executions will be made.\n", host)
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
	// Fail before installing restrictions if the saved origin or credential is invalid.
	if _, err := newMCPGoverned(host); err != nil {
		return fmt.Errorf("connection not ready (use rein login --control-url https://reincontrol.com): %w", err)
	}
	if _, err := registeredAgent(host); err != nil {
		if err := cmdAgent(ctx, []string{"register", host}); err != nil {
			return err
		}
	}
	if err := configurePersistent(host, "", "", true, false, false); err != nil {
		return err
	}
	if err := configurePersistent(host, "", "", false, true, false); err != nil {
		return err
	}
	if err := checkHarnessMCP(ctx, host); err != nil {
		return fmt.Errorf("setup incomplete: MCP verification failed: %w. Settings remain installed; fix the cause and rerun setup, or restore with rein configure %s --persistent --undo", err, host)
	}
	fmt.Printf("Configuration and MCP handshake verified. Runtime enforcement is NOT yet verified. Start %s, trust this project's MCP/settings, inspect /mcp and /hooks, then test native-tool blocking. Publish policy in Rein Control and cache trusted CLI specs with rein spec TOOL before execution tests.\n", harnessBinary(host))
	printPersistentCoverage(host)
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
