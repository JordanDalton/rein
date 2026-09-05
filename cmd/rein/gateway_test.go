package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jordandalton/rein/internal/loop"
)

func startTestGateway(t *testing.T) (string, <-chan error, *gatewayServer) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rein-gateway-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "gateway.sock")
	g := &gatewayServer{opts: gatewayOptions{socket: socket}}
	g.newDeps = func(_ context.Context, caller, cwd string) (*mcpDeps, error) {
		return &mcpDeps{caller: caller, workDir: cwd}, nil
	}
	done := make(chan error, 1)
	go func() { done <- g.serve(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, err := gatewayControl(ctx, socket, gatewayHello{Type: "health"}); err == nil {
			return socket, done, g
		}
		select {
		case err := <-done:
			t.Fatalf("gateway stopped during startup: %v", err)
		case <-ctx.Done():
			t.Fatal("gateway did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func stopTestGateway(t *testing.T, socket string, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := gatewayControl(ctx, socket, gatewayHello{Type: "shutdown"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("gateway did not stop")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("gateway socket was not removed")
	}
}

func TestGatewayBridgeCarriesMCPAndWorkingDirectory(t *testing.T) {
	socket, done, gateway := startTestGateway(t)
	cwd := t.TempDir()
	request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n"
	var output bytes.Buffer
	if err := bridgeGateway(context.Background(), socket, "codex", cwd, strings.NewReader(request), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("missing MCP response: %s", output.String())
	}
	deadline := time.Now().Add(time.Second)
	for gateway.connections.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if gateway.connections.Load() != 0 {
		t.Fatal("closed connection still counted")
	}
	stopTestGateway(t, socket, done)
}

func TestGatewayRejectsInvalidConnectionContext(t *testing.T) {
	socket, done, _ := startTestGateway(t)
	defer stopTestGateway(t, socket, done)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := gatewayControl(ctx, socket, gatewayHello{Type: "connect", Caller: "codex", CWD: "relative"}); err == nil || !strings.Contains(err.Error(), "absolute working directory") {
		t.Fatalf("invalid working directory accepted: %v", err)
	}
	if _, err := gatewayControl(ctx, socket, gatewayHello{Type: "unknown"}); err == nil {
		t.Fatal("unknown control request accepted")
	}
}

func TestGatewaySocketIsPrivate(t *testing.T) {
	socket, done, _ := startTestGateway(t)
	defer stopTestGateway(t, socket, done)
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o", info.Mode().Perm())
	}
	reply, err := gatewayControl(context.Background(), socket, gatewayHello{Type: "health"})
	if err != nil || !reply.OK || reply.PID == 0 {
		data, _ := json.Marshal(reply)
		t.Fatalf("bad health reply %s: %v", data, err)
	}
}

func TestGatewayStartRefusesDifferentRunningSettings(t *testing.T) {
	socket, done, _ := startTestGateway(t)
	defer stopTestGateway(t, socket, done)
	err := startGateway(context.Background(), gatewayOptions{
		socket: socket, ceiling: loop.ApproveAll, steps: 8, timeout: time.Minute, backend: "claude-cli",
	})
	if err == nil || !strings.Contains(err.Error(), "different settings") {
		t.Fatalf("mismatched running gateway accepted: %v", err)
	}
}
