package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// session runs the server over the given request lines and returns each
// response line decoded.
func session(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, r)
	}
	return resps
}

func echoServer(page int) *Server {
	return &Server{
		Name: "bops", Version: "test", PageBytes: page, Keep: 2,
		Tools: []Tool{{Name: "echo", JSON: json.RawMessage(`{"name":"echo","inputSchema":{"type":"object"}}`)}},
		Call: func(_ context.Context, name string, args map[string]any) (string, bool) {
			s, _ := args["text"].(string)
			return s, args["fail"] == true
		},
	}
}

func text(r map[string]any, i int) string {
	c := r["result"].(map[string]any)["content"].([]any)
	if i >= len(c) {
		return ""
	}
	return c[i].(map[string]any)["text"].(string)
}

func TestInitializeListAndCall(t *testing.T) {
	r := session(t, echoServer(0),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"two","method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"text":"no","fail":true}}}`,
	)
	if len(r) != 5 { // the notification gets no response
		t.Fatalf("%d responses: %v", len(r), r)
	}
	init := r[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" || init["serverInfo"].(map[string]any)["name"] != "bops" {
		t.Errorf("initialize = %v", init)
	}
	if r[1]["id"] != "two" {
		t.Errorf("ping id = %v", r[1]["id"])
	}
	if tools := r[2]["result"].(map[string]any)["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Errorf("tools = %v", tools)
	}
	if text(r[3], 0) != "hi" || r[3]["result"].(map[string]any)["isError"] != false {
		t.Errorf("call = %v", r[3])
	}
	if r[4]["result"].(map[string]any)["isError"] != true {
		t.Errorf("failing call = %v", r[4])
	}
}

// An unknown protocol version gets the newest; an unknown method or tool,
// and a line that is not JSON, are JSON-RPC errors.
func TestErrors(t *testing.T) {
	r := session(t, echoServer(0),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope"}}`,
		`not json`,
	)
	if v := r[0]["result"].(map[string]any)["protocolVersion"]; v != ProtocolVersions[0] {
		t.Errorf("version = %v", v)
	}
	for i, code := range []float64{codeMethodNotFound, codeInvalidParams, codeParse} {
		if e, _ := r[i+1]["error"].(map[string]any); e == nil || e["code"] != code {
			t.Errorf("response %d = %v, want code %v", i+1, r[i+1], code)
		}
	}
}

// A result over PageBytes comes back a page at a time, each cut on a UTF-8
// boundary, and the cursor of each page fetches the next.
func TestPagination(t *testing.T) {
	long := strings.Repeat("ab€", 10) // 50 bytes; € is 3 bytes
	s := echoServer(16)
	call := func(args string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":` + args + `}}`
	}
	r := session(t, s, call(`{"text":"`+long+`"}`))
	var got strings.Builder
	for page := 1; ; page++ {
		p := text(r[0], 0)
		if len(p) > 16 || !strings.HasPrefix(long[got.Len():], p) {
			t.Fatalf("page %d = %q", page, p)
		}
		got.WriteString(p)
		note := text(r[0], 1)
		if strings.Contains(note, "the last") {
			break
		}
		i := strings.Index(note, `{"cursor": `)
		if i < 0 || page > 10 {
			t.Fatalf("page %d note = %q", page, note)
		}
		r = session(t, s, call(note[i:len(note)-1]))
	}
	if got.String() != long {
		t.Errorf("pages joined = %q", got.String())
	}
	r = session(t, s, call(`{"cursor":"nope:1"}`))
	if r[0]["result"].(map[string]any)["isError"] != true || !strings.Contains(text(r[0], 0), "unknown or expired cursor") {
		t.Errorf("bad cursor = %v", r[0])
	}
}

// The server keeps Keep paginated results; an older cursor has expired.
func TestPaginationEvictsTheOldest(t *testing.T) {
	s := echoServer(4)
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"0123456789"}}}`
	first := session(t, s, call)
	session(t, s, call)
	session(t, s, call)
	note := text(first[0], 1)
	i := strings.Index(note, `{"cursor": `)
	r := session(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":`+note[i:len(note)-1]+`}}`)
	if !strings.Contains(text(r[0], 0), "expired") {
		t.Errorf("evicted cursor = %v", r[0])
	}
}

func TestSplitKeepsCharactersWhole(t *testing.T) {
	if got := strings.Join(split("€€€", 4), "|"); got != "€|€|€" {
		t.Errorf("split = %q", got)
	}
}
