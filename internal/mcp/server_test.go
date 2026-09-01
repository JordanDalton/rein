package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

type reply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// serve runs a script of client lines through a server and returns every
// response keyed by id.
func serve(t *testing.T, s *Server, script string) map[string]reply {
	t.Helper()
	s.in = strings.NewReader(script)
	out := &bytes.Buffer{}
	s.out = out
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]reply{}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		var r reply
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("unparseable response %q: %v", sc.Text(), err)
		}
		got[string(r.ID)] = r
	}
	return got
}

func echoServer() *Server {
	s := New("rein", "test", nil, nil)
	s.Add(Tool{
		Name:        "echo",
		Description: "returns its input",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var p struct{ Text string }
			json.Unmarshal(args, &p)
			return "echo: " + p.Text, nil
		},
	})
	s.Add(Tool{
		Name: "fail",
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "", io.ErrUnexpectedEOF
		},
	})
	return s
}

func TestHandshakeAndToolCall(t *testing.T) {
	got := serve(t, echoServer(), strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":"s4","method":"ping"}`,
	}, "\n")+"\n")

	if len(got) != 4 {
		t.Fatalf("expected 4 responses (the notification gets none), got %d: %v", len(got), got)
	}

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct{ Name string } `json:"serverInfo"`
	}
	json.Unmarshal(got["1"].Result, &init)
	if init.ProtocolVersion != "2025-06-18" {
		t.Errorf("should echo a known protocol version, got %q", init.ProtocolVersion)
	}
	if init.Capabilities.Tools == nil {
		t.Error("tools capability not advertised")
	}
	if init.ServerInfo.Name != "rein" {
		t.Errorf("serverInfo.name = %q", init.ServerInfo.Name)
	}

	if !strings.Contains(string(got["2"].Result), `"name":"echo"`) {
		t.Errorf("tools/list missing echo: %s", got["2"].Result)
	}
	// A tool registered without a schema still advertises a valid one.
	if !strings.Contains(string(got["2"].Result), `"inputSchema":{"type":"object"}`) {
		t.Errorf("tool without a schema should get a default object schema: %s", got["2"].Result)
	}

	var call struct {
		Content []struct{ Type, Text string }
		IsError bool
	}
	json.Unmarshal(got["3"].Result, &call)
	if len(call.Content) != 1 || call.Content[0].Text != "echo: hi" || call.IsError {
		t.Errorf("tools/call = %+v", call)
	}

	if string(got[`"s4"`].Result) != "{}" {
		t.Errorf("ping = %s", got[`"s4"`].Result)
	}
}

// A handler error is a failed tool call the model can read, not a protocol
// error that aborts the request.
func TestHandlerErrorIsToolError(t *testing.T) {
	got := serve(t, echoServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fail","arguments":{}}}`+"\n")
	r := got["1"]
	if r.Error != nil {
		t.Fatalf("handler error surfaced as protocol error: %v", r.Error)
	}
	var call struct {
		Content []struct{ Text string }
		IsError bool
	}
	json.Unmarshal(r.Result, &call)
	if !call.IsError || !strings.Contains(call.Content[0].Text, "unexpected EOF") {
		t.Errorf("tools/call = %+v", call)
	}
}

func TestProtocolErrors(t *testing.T) {
	got := serve(t, echoServer(), strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
		`this is not json`,
	}, "\n")+"\n")

	if e := got["1"].Error; e == nil || e.Code != codeMethodMissing {
		t.Errorf("unknown method: %v", e)
	}
	if e := got["2"].Error; e == nil || e.Code != codeInvalidParams {
		t.Errorf("unknown tool: %v", e)
	}
	if !strings.Contains(string(got["3"].Result), versions[0]) {
		t.Errorf("unknown client version should be answered with our newest, got %s", got["3"].Result)
	}
	if e := got["null"].Error; e == nil || e.Code != codeParse {
		t.Errorf("parse error: %v", e)
	}
}

// A cancellation notification must reach the handler for the request it
// names, which requires requests to be handled concurrently.
func TestCancelledNotificationStopsHandler(t *testing.T) {
	s := New("rein", "test", nil, nil)
	started := make(chan struct{})
	s.Add(Tool{
		Name: "block",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(started)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
				return "never cancelled", nil
			}
		},
	})

	pr, pw := io.Pipe()
	s.in = pr
	out := &bytes.Buffer{}
	s.out = out
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background()) }()

	io.WriteString(pw, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"block"}}`+"\n")
	<-started
	io.WriteString(pw, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`+"\n")
	pw.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not finish after cancellation")
	}
	if !strings.Contains(out.String(), "context canceled") {
		t.Errorf("handler was not cancelled:\n%s", out.String())
	}
}
