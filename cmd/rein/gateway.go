package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jordandalton/rein/internal/loop"
	"github.com/jordandalton/rein/internal/planner"
	"github.com/jordandalton/rein/internal/spec"
)

const gatewayUsage = `usage:
  rein gateway start [flags]       start the local gateway daemon
  rein gateway serve [flags]       run the gateway in the foreground
  rein gateway status              show whether the gateway is running
  rein gateway stop                stop the local gateway
  rein gateway connect --agent N   MCP stdio bridge used by agent hosts

gateway flags:
  --yes / --auto   maximum local approval level (default: safe)
  --steps N        max planning steps per tool call (default 8)
  --timeout D      per-command timeout (default 60s)
  --backend NAME   planner backend (default claude-cli)
  --model NAME     model for the selected backend
  --base-url URL   override an OpenAI-compatible endpoint
  --api-key-env V  read the model credential from environment variable V
`

type gatewayOptions struct {
	socket  string
	ceiling loop.Approval
	steps   int
	timeout time.Duration
	backend string
	model   string
	baseURL string
	keyEnv  string
}

func defaultGatewaySocket() string        { return filepath.Join(spec.Home(), "gateway.sock") }
func gatewayPIDPath(socket string) string { return socket + ".pid" }
func gatewayLogPath() string              { return filepath.Join(spec.Home(), "gateway.log") }

func cmdGateway(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(gatewayUsage)
		return nil
	}
	switch args[0] {
	case "start", "serve":
		opts, err := parseGatewayOptions(args[0], args[1:])
		if err != nil {
			return err
		}
		if _, err := makeBackend(opts.backend, opts.model, opts.baseURL, opts.keyEnv); err != nil {
			return err
		}
		if args[0] == "serve" {
			return newGatewayServer(opts).serve(ctx)
		}
		return startGateway(ctx, opts)
	case "status", "stop":
		fs := flagSet("gateway " + args[0])
		socket := fs.String("socket", defaultGatewaySocket(), "")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New(gatewayUsage)
		}
		if args[0] == "stop" {
			return stopGateway(ctx, *socket)
		}
		reply, err := gatewayControl(ctx, *socket, gatewayHello{Type: "health"})
		if err != nil {
			return fmt.Errorf("gateway is not running: %w", err)
		}
		printGatewayStatus(reply, *socket)
		return nil
	case "connect":
		fs := flagSet("gateway connect")
		agent := fs.String("agent", "", "")
		socket := fs.String("socket", defaultGatewaySocket(), "")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || strings.TrimSpace(*agent) == "" {
			return errors.New("usage: rein gateway connect --agent <name>")
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
			cwd = resolved
		}
		return bridgeGateway(ctx, *socket, strings.ToLower(*agent), cwd, os.Stdin, os.Stdout)
	default:
		return errors.New(gatewayUsage)
	}
}

func parseGatewayOptions(name string, args []string) (gatewayOptions, error) {
	fs := flag.NewFlagSet("gateway "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := gatewayOptions{}
	yes := fs.Bool("yes", false, "")
	auto := fs.Bool("auto", false, "")
	fs.StringVar(&opts.socket, "socket", defaultGatewaySocket(), "")
	fs.IntVar(&opts.steps, "steps", 8, "")
	fs.DurationVar(&opts.timeout, "timeout", 60*time.Second, "")
	fs.StringVar(&opts.backend, "backend", "claude-cli", "")
	fs.StringVar(&opts.model, "model", "", "")
	fs.StringVar(&opts.baseURL, "base-url", "", "")
	fs.StringVar(&opts.keyEnv, "api-key-env", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*yes && *auto) {
		return opts, errors.New(gatewayUsage)
	}
	opts.ceiling = loop.ApproveSafe
	if *yes {
		opts.ceiling = loop.ApproveCaution
	}
	if *auto {
		opts.ceiling = loop.ApproveAll
	}
	if !filepath.IsAbs(opts.socket) || strings.ContainsAny(opts.socket, "\x00\r\n") {
		return opts, errors.New("gateway socket path must be absolute")
	}
	return opts, nil
}

func (o gatewayOptions) serveArgs() []string {
	args := []string{"gateway", "serve", "--socket", o.socket, "--steps", strconv.Itoa(o.steps), "--timeout", o.timeout.String(), "--backend", o.backend}
	if o.ceiling == loop.ApproveCaution {
		args = append(args, "--yes")
	} else if o.ceiling == loop.ApproveAll {
		args = append(args, "--auto")
	}
	if o.model != "" {
		args = append(args, "--model", o.model)
	}
	if o.baseURL != "" {
		args = append(args, "--base-url", o.baseURL)
	}
	if o.keyEnv != "" {
		args = append(args, "--api-key-env", o.keyEnv)
	}
	return args
}

func startGateway(ctx context.Context, opts gatewayOptions) error {
	return startGatewayOutput(ctx, opts, true)
}

func startGatewayQuiet(ctx context.Context, opts gatewayOptions) error {
	return startGatewayOutput(ctx, opts, false)
}

func startGatewayOutput(ctx context.Context, opts gatewayOptions, verbose bool) error {
	if reply, err := gatewayControl(ctx, opts.socket, gatewayHello{Type: "health"}); err == nil {
		if !reply.matches(opts) {
			return fmt.Errorf("Rein Gateway is already running with different settings (backend %s, approval ceiling %s); stop it before changing gateway settings", reply.Backend, reply.Ceiling)
		}
		if verbose {
			fmt.Println()
			fmt.Println("Rein Gateway")
			fmt.Println("=============")
			fmt.Println("Status: already running")
			fmt.Printf("PID:    %d\n", reply.PID)
			fmt.Printf("Socket: %s\n", opts.socket)
			fmt.Println()
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.socket), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(gatewayLogPath()), 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(gatewayLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	if err := launchGatewayProcess(self, opts.serveArgs(), filepath.Dir(opts.socket), logFile); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if reply, checkErr := gatewayControl(waitCtx, opts.socket, gatewayHello{Type: "health"}); checkErr == nil {
			if verbose {
				fmt.Println()
				fmt.Println("Rein Gateway")
				fmt.Println("=============")
				fmt.Println("Status: started")
				fmt.Printf("PID:    %d\n", reply.PID)
				fmt.Printf("Socket: %s\n", opts.socket)
				fmt.Printf("Logs:   %s\n", gatewayLogPath())
				fmt.Println()
				fmt.Println("Next: configure your host to launch `rein gateway connect --agent NAME`.")
			}
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("gateway did not start; inspect %s", gatewayLogPath())
		case <-ticker.C:
		}
	}
}

func stopGateway(ctx context.Context, socket string) error {
	reply, err := gatewayControl(ctx, socket, gatewayHello{Type: "shutdown"})
	if err != nil {
		return fmt.Errorf("gateway is not running: %w", err)
	}
	fmt.Println()
	fmt.Println("Rein Gateway")
	fmt.Println("=============")
	fmt.Println("Status: stopped")
	fmt.Printf("PID:    %d\n", reply.PID)
	return nil
}

func printGatewayStatus(reply gatewayReply, socket string) {
	fmt.Println()
	fmt.Println("Rein Gateway")
	fmt.Println("=============")
	fmt.Println("Status: running")
	fmt.Printf("PID:    %d\n", reply.PID)
	fmt.Printf("Socket: %s\n", socket)
	fmt.Printf("Connections: %d\n", reply.Connections)
	fmt.Printf("Backend: %s\n", reply.Backend)
	fmt.Printf("Approval ceiling: %s\n", reply.Ceiling)
}

type gatewayHello struct {
	Type   string `json:"type"`
	Caller string `json:"caller,omitempty"`
	CWD    string `json:"cwd,omitempty"`
}

type gatewayReply struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	Version     string `json:"version,omitempty"`
	PID         int    `json:"pid,omitempty"`
	Connections int64  `json:"connections,omitempty"`
	Ceiling     string `json:"approval_ceiling,omitempty"`
	Steps       int    `json:"steps,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	Backend     string `json:"backend,omitempty"`
	Model       string `json:"model,omitempty"`
}

func (r gatewayReply) matches(opts gatewayOptions) bool {
	return r.Ceiling == approvalName(opts.ceiling) && r.Steps == opts.steps && r.Timeout == opts.timeout.String() && r.Backend == opts.backend && r.Model == opts.model
}

func gatewayControl(ctx context.Context, socket string, hello gatewayHello) (gatewayReply, error) {
	var reply gatewayReply
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return reply, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(conn).Encode(hello); err != nil {
		return reply, err
	}
	if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&reply); err != nil {
		return reply, err
	}
	if !reply.OK {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

type gatewayServer struct {
	opts        gatewayOptions
	connections atomic.Int64
	newDeps     func(context.Context, string, string) (*mcpDeps, error)
}

func (g *gatewayServer) reply() gatewayReply {
	return gatewayReply{
		OK: true, Version: version(), PID: os.Getpid(), Connections: g.connections.Load(),
		Ceiling: approvalName(g.opts.ceiling), Steps: g.opts.steps, Timeout: g.opts.timeout.String(),
		Backend: g.opts.backend, Model: g.opts.model,
	}
}

func newGatewayServer(opts gatewayOptions) *gatewayServer {
	g := &gatewayServer{opts: opts}
	g.newDeps = func(ctx context.Context, caller, cwd string) (*mcpDeps, error) {
		d := &mcpDeps{
			caller:  caller,
			workDir: cwd,
			ceiling: opts.ceiling,
			steps:   opts.steps,
			timeout: opts.timeout,
			newSpec: func(ctx context.Context, tool string, refresh bool) (*spec.Spec, error) {
				return ensureSpec(ctx, tool, refresh, spec.Options{})
			},
			newBackend: func() (planner.Backend, error) {
				return makeBackend(opts.backend, opts.model, opts.baseURL, opts.keyEnv)
			},
		}
		governed, err := newMCPGoverned(caller)
		if err != nil {
			return nil, err
		}
		if _, err := governed.bundle(ctx); err != nil {
			return nil, err
		}
		return d, nil
	}
	return g
}

func (g *gatewayServer) serve(parent context.Context) error {
	if err := os.MkdirAll(filepath.Dir(g.opts.socket), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(g.opts.socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket gateway path: %s", g.opts.socket)
		}
		if conn, dialErr := net.DialTimeout("unix", g.opts.socket, 250*time.Millisecond); dialErr == nil {
			conn.Close()
			return errors.New("gateway is already running")
		}
		if err := os.Remove(g.opts.socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", g.opts.socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(g.opts.socket, 0o600); err != nil {
		listener.Close()
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer listener.Close()
	defer os.Remove(g.opts.socket)
	pid := os.Getpid()
	pidData := []byte(strconv.Itoa(pid) + "\n")
	if err := os.WriteFile(gatewayPIDPath(g.opts.socket), pidData, 0o600); err != nil {
		return err
	}
	defer func() {
		if current, err := os.ReadFile(gatewayPIDPath(g.opts.socket)); err == nil && string(current) == string(pidData) {
			_ = os.Remove(gatewayPIDPath(g.opts.socket))
		}
	}()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	fmt.Fprintf(os.Stderr, "rein gateway: serving on %s · backend %s · approval ceiling %s\n", g.opts.socket, g.opts.backend, approvalName(g.opts.ceiling))
	var clients sync.WaitGroup
	defer clients.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		clients.Add(1)
		go func() {
			defer clients.Done()
			g.serveConn(ctx, cancel, conn)
		}()
	}
}

func (g *gatewayServer) serveConn(ctx context.Context, shutdown context.CancelFunc, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, 64<<10)
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > 64<<10 {
		writeGatewayReply(conn, gatewayReply{Error: "invalid gateway handshake"})
		return
	}
	var hello gatewayHello
	if json.Unmarshal(line, &hello) != nil {
		writeGatewayReply(conn, gatewayReply{Error: "invalid gateway handshake"})
		return
	}
	switch hello.Type {
	case "health":
		writeGatewayReply(conn, g.reply())
		return
	case "shutdown":
		writeGatewayReply(conn, g.reply())
		shutdown()
		return
	case "connect":
	default:
		writeGatewayReply(conn, gatewayReply{Error: "unknown gateway request"})
		return
	}
	hello.Caller = strings.ToLower(strings.TrimSpace(hello.Caller))
	if hello.Caller == "" || strings.ContainsAny(hello.Caller, " /\\\x00\r\n") {
		writeGatewayReply(conn, gatewayReply{Error: "a valid registered caller is required"})
		return
	}
	if !filepath.IsAbs(hello.CWD) || strings.ContainsAny(hello.CWD, "\x00\r\n") {
		writeGatewayReply(conn, gatewayReply{Error: "an absolute working directory is required"})
		return
	}
	info, err := os.Stat(hello.CWD)
	if err != nil || !info.IsDir() {
		writeGatewayReply(conn, gatewayReply{Error: "working directory is unavailable"})
		return
	}
	d, err := g.newDeps(ctx, hello.Caller, hello.CWD)
	if err != nil {
		writeGatewayReply(conn, gatewayReply{Error: err.Error()})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	g.connections.Add(1)
	defer g.connections.Add(-1)
	writeGatewayReply(conn, g.reply())
	clientCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-clientCtx.Done()
		_ = conn.Close()
	}()
	_ = newReinMCPServer(d, reader, conn).Serve(clientCtx)
}

func writeGatewayReply(w io.Writer, reply gatewayReply) {
	_ = json.NewEncoder(w).Encode(reply)
}

func bridgeGateway(ctx context.Context, socket, caller, cwd string, input io.Reader, output io.Writer) error {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("cannot connect to Rein Gateway; run `rein gateway start`: %w", err)
	}
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 64<<10)
	if err := json.NewEncoder(conn).Encode(gatewayHello{Type: "connect", Caller: caller, CWD: cwd}); err != nil {
		return err
	}
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > 64<<10 {
		return errors.New("gateway did not complete the handshake")
	}
	var reply gatewayReply
	if json.Unmarshal(line, &reply) != nil || !reply.OK {
		if reply.Error != "" {
			return errors.New(reply.Error)
		}
		return errors.New("gateway rejected the connection")
	}
	type copyResult struct {
		direction string
		err       error
	}
	done := make(chan copyResult, 2)
	go func() {
		_, copyErr := io.Copy(conn, input)
		if unixConn, ok := conn.(*net.UnixConn); ok {
			_ = unixConn.CloseWrite()
		}
		done <- copyResult{direction: "input", err: copyErr}
	}()
	go func() {
		_, copyErr := io.Copy(output, reader)
		done <- copyResult{direction: "output", err: copyErr}
	}()
	for completed := 0; completed < 2; completed++ {
		select {
		case <-ctx.Done():
			return nil
		case result := <-done:
			if result.err != nil && !errors.Is(result.err, net.ErrClosed) {
				return fmt.Errorf("gateway %s bridge failed: %w", result.direction, result.err)
			}
			if result.direction == "output" {
				return nil
			}
		}
	}
	return nil
}
