package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/bigquery-mcp/internal/config"
	"github.com/nlink-jp/bigquery-mcp/internal/mcpserver"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
	"github.com/nlink-jp/bigquery-mcp/internal/transport"
)

// listedTool is one entry of a tools/list reply.
type listedTool struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"inputSchema"`
}

// registeredTools drives a real tools/list through a server that Register
// populated, so what comes back is the production registry itself rather than
// a second, hand-written copy of it that could drift.
//
// A nil *bq.Client is enough here — registration only records the descriptors,
// and tools/list never enters a handler.
func registeredTools(t *testing.T) []listedTool {
	t.Helper()
	in := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := mcpserver.New("bigquery-mcp", "test", transport.NewStdioTransport(in, &out), logger)
	Register(srv, nil, config.Default(), logger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Serve(ctx); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resp struct {
		Result struct {
			Tools []listedTool `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/list reply %q: %v", out.String(), err)
	}
	if resp.Error != nil {
		t.Fatalf("tools/list returned an error: %s", resp.Error.Message)
	}
	return resp.Result.Tools
}

// TestEveryToolSchemaIsClosed is the arch test organization ADR-021 §10
// requires: every registered tool's TOP-LEVEL input schema sets
// additionalProperties:false, so a client validating arguments against the
// schema refuses a mistyped parameter instead of sending it on.
//
// Top-level is the whole claim. The nested `params` object of dry_run and
// query is deliberately open, because its keys are the caller's own query
// parameters; TestParamsObjectStaysOpen guards that exception. Decoding into
// a struct here reads the top-level key only, so the nested one cannot
// accidentally satisfy this test.
//
// The server half of the contract is real too: parseArgs sets
// DisallowUnknownFields, and TestUnknownArgumentIsRejected exercises it end
// to end.
func TestEveryToolSchemaIsClosed(t *testing.T) {
	tools := registeredTools(t)
	// Vacuity guard: with an empty list every assertion below passes without
	// having examined anything.
	if len(tools) == 0 {
		t.Fatal("tools/list advertises no tools, so this test proves nothing")
	}
	for _, tl := range tools {
		var schema struct {
			Type                 string `json:"type"`
			AdditionalProperties *bool  `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tl.Schema, &schema); err != nil {
			t.Errorf("tool %q: input schema is not valid JSON: %v", tl.Name, err)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("tool %q: schema type = %q, want object", tl.Name, schema.Type)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			got := "absent"
			if schema.AdditionalProperties != nil {
				got = "true"
			}
			t.Errorf("tool %q: top-level input schema does not set "+
				"additionalProperties:false (%s) — a validating client would pass "+
				"an agent's mistyped argument through unnoticed (organization "+
				"ADR-021 §10)", tl.Name, got)
		}
	}
}

// TestParamsObjectStaysOpen protects the one deliberate exception from a
// future sweep that closes everything it finds. `params` carries the caller's
// own named query parameters (@name in the SQL), which no schema can
// enumerate, so closing it would reject every parameterised query.
func TestParamsObjectStaysOpen(t *testing.T) {
	var found int
	for _, tl := range registeredTools(t) {
		var schema struct {
			Properties struct {
				Params *struct {
					Type                 string `json:"type"`
					AdditionalProperties *bool  `json:"additionalProperties"`
				} `json:"params"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tl.Schema, &schema); err != nil {
			t.Fatalf("tool %q: input schema is not valid JSON: %v", tl.Name, err)
		}
		p := schema.Properties.Params
		if p == nil {
			continue
		}
		found++
		if p.AdditionalProperties == nil || !*p.AdditionalProperties {
			t.Errorf("tool %q: params must keep additionalProperties:true — its "+
				"keys are the caller's own query parameter names and cannot be "+
				"enumerated; closing it rejects every parameterised query", tl.Name)
		}
	}
	// Vacuity guard: the two tools that take params must have been seen.
	if found != 2 {
		t.Fatalf("found %d tools exposing params, want 2 (dry_run and query) — "+
			"this test may be checking nothing", found)
	}
}

// TestUnknownArgumentIsRejected proves the strictness is real and not merely
// declared: the closed schema stops a typo at a validating client, and
// parseArgs' DisallowUnknownFields stops it here, for any caller that speaks
// JSON-RPC directly. A misspelled max_rows must not silently fall back to the
// configured default.
func TestUnknownArgumentIsRejected(t *testing.T) {
	h := newHarness(t)
	text, isErr := h.call("query", `{"query":"SELECT 1","max_rowz":10}`)
	if !isErr {
		t.Fatalf("a misspelled argument was accepted: %s", text)
	}
	if !strings.Contains(text, toolerr.CodeInvalidArguments) {
		t.Errorf("error code is not %s: %s", toolerr.CodeInvalidArguments, text)
	}
	if !strings.Contains(text, "max_rowz") {
		t.Errorf("the error does not name the offending field: %s", text)
	}
}
