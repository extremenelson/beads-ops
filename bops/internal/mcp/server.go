// Package mcp is a minimal Model Context Protocol server over stdio:
// JSON-RPC 2.0, one message per line, with the tools capability only
// (initialize, ping, tools/list, tools/call). It is standard library only
// (SPEC §11a); the tools and what calling one does come from the caller.
package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// ProtocolVersions are the MCP revisions this server speaks, newest first.
// It answers initialize with the client's version when it is one of these,
// and with the newest otherwise, which the client may then refuse.
var ProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Tool is one tool as tools/list returns it.
type Tool struct {
	Name string
	JSON json.RawMessage // the whole tool object
}

// CallFunc runs a tool with its arguments and returns its result text and
// whether that result is an error.
type CallFunc func(ctx context.Context, name string, args map[string]any) (text string, isError bool)

// Server serves Tools over stdio, running each call through Call.
type Server struct {
	Name, Version string
	Tools         []Tool
	Call          CallFunc
	PageBytes     int // a result longer than this is paginated
	// Keep is how many paginated results the server holds for their
	// cursors; the oldest is dropped first.
	Keep int

	mu    sync.Mutex
	pages map[string][]string
	order []string
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Serve reads requests from r and writes responses to w until r ends or ctx
// is done. Requests are answered one at a time, in order. Notifications
// (requests without an id) get no response.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	enc := json.NewEncoder(w)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			if resp := s.handle(ctx, line); resp != nil {
				if werr := enc.Encode(resp); werr != nil {
					return werr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) handle(ctx context.Context, line []byte) *response {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return &response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "parse error: " + err.Error()}}
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if len(req.ID) == 0 {
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{codeInvalidRequest, "not a JSON-RPC 2.0 request"}}
	}
	if len(req.ID) == 0 { // a notification: notifications/initialized, cancelled
		return nil
	}
	result, rerr := s.dispatch(ctx, req)
	if rerr != nil {
		return &response{JSONRPC: "2.0", ID: req.ID, Error: rerr}
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := ProtocolVersions[0]
		if slices.Contains(ProtocolVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := make([]json.RawMessage, len(s.Tools))
		for i, t := range s.Tools {
			tools[i] = t.JSON
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{codeInvalidParams, "tools/call: " + err.Error()}
		}
		if !slices.ContainsFunc(s.Tools, func(t Tool) bool { return t.Name == p.Name }) {
			return nil, &rpcError{codeInvalidParams, "unknown tool " + p.Name}
		}
		return s.call(ctx, p.Name, p.Arguments), nil
	}
	return nil, &rpcError{codeMethodNotFound, "method not found: " + req.Method}
}

// call runs a tool, or serves the next page of an earlier result when the
// arguments carry a cursor.
func (s *Server) call(ctx context.Context, name string, args map[string]any) map[string]any {
	if c, ok := args["cursor"].(string); ok && c != "" {
		return s.page(name, c)
	}
	delete(args, "cursor")
	text, isError := s.Call(ctx, name, args)
	if s.PageBytes <= 0 || len(text) <= s.PageBytes {
		return result(isError, text)
	}
	pages := split(text, s.PageBytes)
	id := s.keep(pages)
	return result(isError, pages[0], s.more(name, id, 1, len(pages)))
}

func (s *Server) page(name, cursor string) map[string]any {
	id, n, ok := parseCursor(cursor)
	s.mu.Lock()
	pages := s.pages[id]
	s.mu.Unlock()
	if !ok || pages == nil || n < 1 || n >= len(pages) {
		return result(true, fmt.Sprintf("unknown or expired cursor %q: run %s again without one", cursor, name))
	}
	if n == len(pages)-1 {
		return result(false, pages[n], fmt.Sprintf("Page %d of %d, the last.", n+1, len(pages)))
	}
	return result(false, pages[n], s.more(name, id, n+1, len(pages)))
}

func (s *Server) more(name, id string, next, of int) string {
	return fmt.Sprintf("Page %d of %d of a result over %d bytes. For the next page, call %s with {\"cursor\": %q}.", next, of, s.PageBytes, name, fmt.Sprintf("%s:%d", id, next))
}

func (s *Server) keep(pages []string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pages == nil {
		s.pages = map[string][]string{}
	}
	s.pages[id] = pages
	s.order = append(s.order, id)
	keep := max(s.Keep, 1)
	for len(s.order) > keep {
		delete(s.pages, s.order[0])
		s.order = s.order[1:]
	}
	return id
}

func parseCursor(c string) (string, int, bool) {
	id, ns, ok := strings.Cut(c, ":")
	var n int
	if !ok || id == "" {
		return "", 0, false
	}
	if _, err := fmt.Sscanf(ns, "%d", &n); err != nil {
		return "", 0, false
	}
	return id, n, true
}

// split cuts text into pieces of at most size bytes, never inside a UTF-8
// character.
func split(text string, size int) []string {
	var out []string
	for len(text) > size {
		cut := size
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			cut = size
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	return append(out, text)
}

func result(isError bool, texts ...string) map[string]any {
	content := make([]map[string]any, len(texts))
	for i, t := range texts {
		content[i] = map[string]any{"type": "text", "text": t}
	}
	return map[string]any{"content": content, "isError": isError}
}
