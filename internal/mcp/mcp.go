// Package mcp is a minimal Model Context Protocol server: newline-delimited
// JSON-RPC 2.0 over a stream (stdio framing), serving tools only. The daemon
// serves it on a Unix socket mounted into the orchestrator's container, and
// `factory mcp-bridge` connects the agent's stdio to that socket.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2025-06-18"

// Tool is one callable tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// Handler returns text for the agent, or an error shown to it as a tool
	// error (isError), never as a protocol error.
	Handler func(ctx context.Context, args json.RawMessage) (string, error) `json:"-"`
}

// Server serves a fixed set of tools.
type Server struct {
	Name  string
	Tools []Tool
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve handles one connection until EOF or ctx ends.
func (s *Server) Serve(ctx context.Context, rw io.ReadWriter) error {
	sc := bufio.NewScanner(rw)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var mu sync.Mutex
	enc := json.NewEncoder(rw)
	write := func(r response) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(r)
	}
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			write(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification, e.g. notifications/initialized
		}
		res, rerr := s.handle(ctx, req)
		write(response{JSONRPC: "2.0", ID: req.ID, Result: res, Error: rerr})
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": "1"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.Tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		for _, t := range s.Tools {
			if t.Name != p.Name {
				continue
			}
			if len(p.Arguments) == 0 {
				p.Arguments = json.RawMessage("{}")
			}
			text, err := t.Handler(ctx, p.Arguments)
			if err != nil {
				return toolResult(err.Error(), true), nil
			}
			return toolResult(text, false), nil
		}
		return nil, &rpcError{-32602, fmt.Sprintf("unknown tool %q", p.Name)}
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
}

// ListenUnix serves the tools on a Unix socket until ctx ends.
func (s *Server) ListenUnix(ctx context.Context, path string) (net.Listener, error) {
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_ = s.Serve(ctx, c)
			}()
		}
	}()
	return l, nil
}

// Bridge copies stdin to the socket and the socket to stdout: it is the
// stdio MCP server the agent launches inside its container.
func Bridge(socket string, in io.Reader, out io.Writer) error {
	c, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, c)
		done <- err
	}()
	if _, err := io.Copy(c, in); err != nil {
		return err
	}
	if uc, ok := c.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	return <-done
}
