// Package mcp is a minimal Model Context Protocol server over stdio: enough
// JSON-RPC and enough of the "tools" capability for an agent to call rein.
//
// It deliberately implements nothing else. Resources, prompts, sampling and
// the HTTP transport are all out of scope; a client that needs them will
// negotiate them away at initialize.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Tool is one callable exposed to the client. Handler returns the text to
// show the model; a non-nil error is reported as a failed tool call rather
// than a protocol error, so the model sees the message and can recover.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server speaks newline-delimited JSON-RPC on one reader/writer pair.
type Server struct {
	Name         string
	Version      string
	Instructions string

	in    io.Reader
	out   io.Writer
	tools []Tool

	wmu      sync.Mutex // serialises writes to out
	imu      sync.Mutex // guards inflight
	inflight map[string]context.CancelFunc
}

// New returns a server for the given transport. Stdio servers pass os.Stdin
// and os.Stdout; everything the server wants to log must go elsewhere.
func New(name, version string, in io.Reader, out io.Writer) *Server {
	return &Server{
		Name:     name,
		Version:  version,
		in:       in,
		out:      out,
		inflight: map[string]context.CancelFunc{},
	}
}

// Add registers a tool.
func (s *Server) Add(t Tool) { s.tools = append(s.tools, t) }

// Protocol versions this server can speak. The newest is offered when the
// client asks for one we do not know.
var versions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParse         = -32700
	codeInvalidReq    = -32600
	codeMethodMissing = -32601
	codeInvalidParams = -32602
)

// Serve reads requests until the input closes or ctx is cancelled. Requests
// are handled concurrently so a long tool call does not block a ping or a
// cancellation notification; responses are written whole, one per line.
func (s *Server) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	r := bufio.NewReaderSize(s.in, 1<<20)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			s.dispatch(ctx, &wg, line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (s *Server) dispatch(ctx context.Context, wg *sync.WaitGroup, line []byte) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		s.write(message{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{codeParse, "parse error: " + err.Error()}})
		return
	}
	if m.Method == "" {
		return // a response to a request we never sent; ignore
	}
	if len(m.ID) == 0 || string(m.ID) == "null" {
		s.notify(m)
		return
	}

	rctx, rcancel := context.WithCancel(ctx)
	key := string(m.ID)
	s.imu.Lock()
	s.inflight[key] = rcancel
	s.imu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			s.imu.Lock()
			delete(s.inflight, key)
			s.imu.Unlock()
			rcancel()
		}()
		result, rerr := s.handle(rctx, m)
		resp := message{JSONRPC: "2.0", ID: m.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		s.write(resp)
	}()
}

func (s *Server) notify(m message) {
	if m.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	s.imu.Lock()
	cancel := s.inflight[string(p.RequestID)]
	s.imu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) handle(ctx context.Context, m message) (any, *rpcError) {
	switch m.Method {
	case "initialize":
		return s.initialize(m.Params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.list(), nil
	case "tools/call":
		return s.call(ctx, m.Params)
	default:
		return nil, &rpcError{codeMethodMissing, "method not found: " + m.Method}
	}
}

func (s *Server) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	v := versions[0]
	for _, known := range versions {
		if known == p.ProtocolVersion {
			v = known
			break
		}
	}
	res := map[string]any{
		"protocolVersion": v,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
	}
	if s.Instructions != "" {
		res["instructions"] = s.Instructions
	}
	return res
}

func (s *Server) list() any {
	type tool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	out := make([]tool, 0, len(s.tools))
	for _, t := range s.tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, tool{t.Name, t.Description, schema})
	}
	return map[string]any{"tools": out}
}

func (s *Server) call(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{codeInvalidParams, "bad params: " + err.Error()}
	}
	var t *Tool
	for i := range s.tools {
		if s.tools[i].Name == p.Name {
			t = &s.tools[i]
		}
	}
	if t == nil {
		return nil, &rpcError{codeInvalidParams, fmt.Sprintf("unknown tool %q", p.Name)}
	}
	if len(p.Arguments) == 0 {
		p.Arguments = json.RawMessage(`{}`)
	}
	text, err := t.Handler(ctx, p.Arguments)
	if err != nil {
		return textResult(err.Error(), true), nil
	}
	return textResult(text, false), nil
}

func textResult(text string, isErr bool) any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

func (s *Server) write(m message) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.out.Write(append(b, '\n'))
}
