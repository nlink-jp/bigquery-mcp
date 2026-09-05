package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/nlink-jp/bigquery-mcp/internal/transport"
)

// TestBasicRoundTrip drives the server with a canned sequence of stdio
// messages and checks that initialize / notifications/initialized / tools/list
// / tools/call round-trip correctly. Bytes.Buffer reads EOF after exhaustion,
// which causes Serve to return cleanly.
func TestBasicRoundTrip(t *testing.T) {
	in := bytes.NewBufferString(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hello"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"missing","arguments":{}}}`,
	}, "\n") + "\n")

	var out bytes.Buffer
	tr := transport.NewStdioTransport(in, &out)
	srv := New("bigquery-mcp", "test", tr, slog.New(slog.NewTextHandler(io.Discard, nil)))

	srv.RegisterTool(Tool{
		Name:        "echo",
		Description: "Echo the input msg",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
	}, func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(args, &in)
		return map[string]string{"echoed": in.Msg}, nil
	})

	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("serve returned error: %v", err)
	}

	lines := splitLines(out.String())

	// Notifications get no response, so we expect 4 responses for IDs 1,2,3,4.
	if got, want := len(lines), 4; got != want {
		t.Fatalf("got %d response lines, want %d\nout:\n%s", got, want, out.String())
	}

	// initialize response includes the protocol version and server info.
	if !strings.Contains(lines[0], `"protocolVersion":"2024-11-05"`) {
		t.Errorf("initialize response missing protocolVersion: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"name":"bigquery-mcp"`) {
		t.Errorf("initialize response missing server name: %s", lines[0])
	}

	// tools/list response includes the registered echo tool.
	if !strings.Contains(lines[1], `"name":"echo"`) {
		t.Errorf("tools/list response missing echo: %s", lines[1])
	}

	// tools/call echo returns the input message.
	if !strings.Contains(lines[2], `hello`) {
		t.Errorf("tools/call echo did not echo hello: %s", lines[2])
	}
	if strings.Contains(lines[2], `"isError":true`) {
		t.Errorf("tools/call echo unexpectedly flagged isError: %s", lines[2])
	}

	// Unknown tool returns a JSON-RPC error (method not found).
	if !strings.Contains(lines[3], `"error"`) {
		t.Errorf("unknown tool call did not return JSON-RPC error: %s", lines[3])
	}
	if !strings.Contains(lines[3], `unknown tool`) {
		t.Errorf("unknown tool error missing expected message: %s", lines[3])
	}
}

// TestParseError checks that malformed JSON gets a parse-error response with id=null.
func TestParseError(t *testing.T) {
	in := bytes.NewBufferString("not-json\n")
	var out bytes.Buffer
	tr := transport.NewStdioTransport(in, &out)
	srv := New("bigquery-mcp", "test", tr, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}

	if !strings.Contains(out.String(), `"code":-32700`) {
		t.Errorf("expected parse-error code -32700; got: %s", out.String())
	}
	if !strings.Contains(out.String(), `"id":null`) {
		t.Errorf("expected id:null on parse error; got: %s", out.String())
	}
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
