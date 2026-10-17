package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func server() *Server {
	return &Server{Name: "factory", Tools: []Tool{
		{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(_ context.Context, a json.RawMessage) (string, error) { return string(a), nil }},
		{Name: "nope", Description: "fails", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("refused: over cap") }},
	}}
}

func roundTrip(t *testing.T, rw io.ReadWriter, out *bufio.Scanner, msg string) map[string]any {
	t.Helper()
	if _, err := io.WriteString(rw, msg+"\n"); err != nil {
		t.Fatal(err)
	}
	if !out.Scan() {
		t.Fatalf("no response to %s", msg)
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestProtocolOverUnixSocketAndBridge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	if _, err := server().ListenUnix(ctx, sock); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go Bridge(sock, inR, outW)
	sc := bufio.NewScanner(outR)
	rw := struct {
		io.Reader
		io.Writer
	}{outR, inW}

	init := roundTrip(t, rw, sc, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if init["result"].(map[string]any)["protocolVersion"] != ProtocolVersion {
		t.Fatalf("init = %v", init)
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	list := roundTrip(t, rw, sc, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if n := len(list["result"].(map[string]any)["tools"].([]any)); n != 2 {
		t.Fatalf("tools = %d", n)
	}
	call := roundTrip(t, rw, sc, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`)
	if !strings.Contains(mustJSON(call), `{\"x\":1}`) {
		t.Fatalf("call = %v", call)
	}
	refused := roundTrip(t, rw, sc, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`)
	r := refused["result"].(map[string]any)
	if r["isError"] != true || !strings.Contains(mustJSON(r), "over cap") {
		t.Fatalf("refused = %v", refused)
	}
	unknown := roundTrip(t, rw, sc, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"merge_pr"}}`)
	if unknown["error"] == nil {
		t.Fatalf("unknown tool should be a protocol error: %v", unknown)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
