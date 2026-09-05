package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
	"github.com/nlink-jp/bigquery-mcp/internal/config"
	"github.com/nlink-jp/bigquery-mcp/internal/mcpserver"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
	"github.com/nlink-jp/bigquery-mcp/internal/transport"
)

// fakeBQ scripts BigQuery responses per "METHOD path" and records calls.
type fakeBQ struct {
	handlers map[string]func(w http.ResponseWriter, r *http.Request)
	calls    []string
}

func (f *fakeBQ) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if cfg, ok := body["configuration"].(map[string]any); ok && cfg["dryRun"] == true {
			key = "DRYRUN " + r.URL.Path
		} else {
			key = "INSERT " + r.URL.Path
		}
	}
	f.calls = append(f.calls, key)
	if h, ok := f.handlers[key]; ok {
		h(w, r)
		return
	}
	http.Error(w, `{"error":{"code":404,"message":"no handler for `+key+`","errors":[{"reason":"notFound"}]}}`, 404)
}

func (f *fakeBQ) reply(method, path string, status int, body string) {
	f.handlers[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func (f *fakeBQ) count(key string) int {
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

// harness runs tools through the real mcpserver so the JSON contract is
// what a client sees.
type harness struct {
	t    *testing.T
	fake *fakeBQ
	cfg  *config.Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.ProjectID = "billing"
	return &harness{t: t, fake: &fakeBQ{handlers: map[string]func(http.ResponseWriter, *http.Request){}}, cfg: cfg}
}

// call sends one tools/call and returns the text of the first content
// block and the isError flag.
func (h *harness) call(tool string, args string) (string, bool) {
	h.t.Helper()
	srv := httptest.NewServer(h.fake)
	defer srv.Close()
	client := bq.NewForTest(srv.URL, srv.Client(), h.cfg.ProjectID, h.cfg.Location)

	in := bytes.NewBufferString(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", tool, args))
	var out bytes.Buffer
	tr := transport.NewStdioTransport(in, &out)
	s := mcpserver.New("bigquery-mcp", "test", tr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	Register(s, client, h.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Serve(ctx); err != nil {
		h.t.Fatalf("serve: %v", err)
	}
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		h.t.Fatalf("decode response %q: %v", out.String(), err)
	}
	if resp.Error != nil {
		h.t.Fatalf("JSON-RPC error: %s", resp.Error.Message)
	}
	if len(resp.Result.Content) == 0 {
		h.t.Fatalf("no content in %s", out.String())
	}
	return resp.Result.Content[0].Text, resp.Result.IsError
}

func (h *harness) callJSON(tool, args string) (map[string]any, bool) {
	h.t.Helper()
	text, isErr := h.call(tool, args)
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		h.t.Fatalf("result is not JSON: %q", text)
	}
	return m, isErr
}

func (h *harness) expectError(tool, args, code string) map[string]any {
	h.t.Helper()
	m, isErr := h.callJSON(tool, args)
	if !isErr {
		h.t.Fatalf("expected isError for %s %s, got %v", tool, args, m)
	}
	if m["code"] != code {
		h.t.Fatalf("expected code %s, got %v (%v)", code, m["code"], m["message"])
	}
	if _, ok := m["retryable"]; !ok {
		h.t.Fatalf("retryable must always be present: %v", m)
	}
	return m
}

const dryRunOK = `{"jobReference":{"projectId":"billing","jobId":"dry","location":"US"},"status":{"state":"DONE"},
	"statistics":{"query":{"statementType":"SELECT","totalBytesProcessed":"1000","referencedTables":[{"projectId":"p","datasetId":"d","tableId":"t"}],
	"schema":{"fields":[{"name":"email","type":"STRING"},{"name":"cnt","type":"INTEGER"}]}}}}`

func rowsJSON(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"f":[{"v":"u%d@x"},{"v":"%d"}]}`, i, i)
	}
	return b.String()
}

// runOK scripts the three calls of a successful run: the named job insert,
// one results page, and the statistics read.
func (h *harness) runOK(n int, total int) {
	h.fake.reply("INSERT", "/projects/billing/jobs", 200, `{"jobReference":{"projectId":"billing","jobId":"bqmcp-test-1","location":"US"},"status":{"state":"RUNNING"}}`)
	h.fake.reply("GET", "/projects/billing/queries/bqmcp-test-1", 200, fmt.Sprintf(`{"jobReference":{"jobId":"bqmcp-test-1","location":"US"},"jobComplete":true,
		"schema":{"fields":[{"name":"email","type":"STRING"},{"name":"cnt","type":"INTEGER"}]},"rows":[%s],"totalRows":"%d","totalBytesProcessed":"1000"}`, rowsJSON(n), total))
	h.fake.reply("GET", "/projects/billing/jobs/bqmcp-test-1", 200, `{"statistics":{"query":{"statementType":"SELECT","totalBytesBilled":"10485760","cacheHit":false,"totalSlotMs":"3"}}}`)
}

func TestQueryHappyPathWithWarning(t *testing.T) {
	h := newHarness(t)
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, dryRunOK)
	// The table is 1000 bytes and the estimate is 1000 bytes: nothing was pruned.
	h.fake.reply("GET", "/projects/p/datasets/d/tables/t", 200, `{"tableReference":{"projectId":"p","datasetId":"d","tableId":"t"},"numBytes":"1000","timePartitioning":{"type":"DAY","field":"event_date"}}`)
	h.runOK(2, 2)
	m, isErr := h.callJSON("query", `{"query":"SELECT email, COUNT(*) cnt FROM p.d.t GROUP BY 1"}`)
	if isErr {
		t.Fatalf("unexpected error: %v", m)
	}
	rows := m["rows"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["email"] != "u0@x" || rows[1].(map[string]any)["cnt"] != float64(1) {
		t.Errorf("rows = %v", rows)
	}
	if m["truncated"] != false || m["total_rows"] != float64(2) || m["job_id"] != "bqmcp-test-1" || m["bytes_billed"] != float64(10485760) {
		t.Errorf("meta = %v", m)
	}
	w := m["warnings"].([]any)
	if len(w) != 1 || !strings.Contains(w[0].(string), "event_date") {
		t.Errorf("expected a partition warning, got %v", w)
	}
	if h.fake.calls[0] != "DRYRUN /projects/billing/jobs" || h.fake.count("INSERT /projects/billing/jobs") != 1 {
		t.Errorf("dry run must precede the run: %v", h.fake.calls)
	}
}

func TestQueryNoWarningWhenPartitionsWerePruned(t *testing.T) {
	h := newHarness(t)
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, dryRunOK)
	// The table is 50 KB and the estimate is 1000 bytes: the filter pruned.
	h.fake.reply("GET", "/projects/p/datasets/d/tables/t", 200, `{"tableReference":{"projectId":"p","datasetId":"d","tableId":"t"},"numBytes":"50000","timePartitioning":{"type":"DAY","field":"event_date"}}`)
	h.runOK(1, 1)
	m, _ := h.callJSON("query", `{"query":"SELECT 1 FROM p.d.t WHERE event_date = '2026-09-05'"}`)
	if _, has := m["warnings"]; has {
		t.Errorf("no warning expected when the estimate is below the table size: %v", m["warnings"])
	}
}

func TestImpreciseEstimateWarns(t *testing.T) {
	h := newHarness(t)
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, strings.Replace(dryRunOK, `"totalBytesProcessed":"1000"`, `"totalBytesProcessed":"1000","totalBytesProcessedAccuracy":"LOWER_BOUND"`, 1))
	h.fake.reply("GET", "/projects/p/datasets/d/tables/t", 200, `{"numBytes":"50000"}`)
	m, _ := h.callJSON("dry_run", `{"query":"SELECT 1 FROM p.d.t"}`)
	w, _ := m["warnings"].([]any)
	if len(w) != 1 || !strings.Contains(w[0].(string), "LOWER_BOUND") || m["accuracy"] != "LOWER_BOUND" {
		t.Errorf("%v", m)
	}
}

func TestGateRefusesNonSelectBeforeRunning(t *testing.T) {
	h := newHarness(t)
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, strings.Replace(dryRunOK, `"SELECT"`, `"SCRIPT"`, 1))
	m := h.expectError("query", `{"query":"BEGIN SELECT 1; END"}`, toolerr.CodeStatementNotAllowed)
	if m["details"].(map[string]any)["statement_type"] != "SCRIPT" {
		t.Errorf("details = %v", m["details"])
	}
	if h.fake.count("INSERT /projects/billing/jobs") != 0 {
		t.Errorf("a refused statement must never reach jobs.query: %v", h.fake.calls)
	}
}

func TestGateRefusesOverBudget(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxBytesBilled = config.MinBytesBilled                                                                                                            // 10 MiB
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, strings.Replace(dryRunOK, `"totalBytesProcessed":"1000"`, `"totalBytesProcessed":"10485761"`, 1)) // one byte over: rounds up to 11 MiB
	m := h.expectError("query", `{"query":"SELECT * FROM p.d.t"}`, toolerr.CodeBudgetExceeded)
	d := m["details"].(map[string]any)
	if d["bytes_processed"] != float64(10485761) || d["bytes_billed_estimate"] != float64(11<<20) || d["budget_bytes"] != float64(config.MinBytesBilled) || m["retryable"] != false {
		t.Errorf("%v", m)
	}
	if h.fake.count("INSERT /projects/billing/jobs") != 0 {
		t.Errorf("over-budget query must not run")
	}
}

func TestGateEnforcesAllowlist(t *testing.T) {
	h := newHarness(t)
	h.cfg.Datasets = []string{"p.other", "q.*"}
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, dryRunOK) // references p.d.t
	m := h.expectError("query", `{"query":"SELECT 1 FROM p.d.t"}`, toolerr.CodeDatasetNotAllowed)
	if m["details"].(map[string]any)["table"] != "p.d.t" {
		t.Errorf("%v", m)
	}
	// Wildcard project passes.
	h.cfg.Datasets = []string{"p.*"}
	h.fake.reply("GET", "/projects/p/datasets/d/tables/t", 200, `{}`)
	h.runOK(1, 1)
	if _, isErr := h.callJSON("query", `{"query":"SELECT 1 FROM p.d.t"}`); isErr {
		t.Errorf("p.* should allow p.d.t")
	}
	// A routine outside the list is refused even when its tables are inside.
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, strings.Replace(dryRunOK, `"referencedTables"`, `"referencedRoutines":[{"projectId":"q","datasetId":"fn","routineId":"tvf"}],"referencedTables"`, 1))
	m = h.expectError("query", `{"query":"SELECT * FROM q.fn.tvf()"}`, toolerr.CodeDatasetNotAllowed)
	if m["details"].(map[string]any)["routine"] != "q.fn.tvf" {
		t.Errorf("%v", m)
	}
}

func TestTruncationByMaxRows(t *testing.T) {
	h := newHarness(t)
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, dryRunOK)
	h.fake.reply("GET", "/projects/p/datasets/d/tables/t", 200, `{}`)
	h.runOK(10, 10)
	m, isErr := h.callJSON("query", `{"query":"SELECT * FROM p.d.t","max_rows":3}`)
	if isErr {
		t.Fatalf("%v", m)
	}
	if m["truncated"] != true || m["truncated_by"] != "max_rows" || m["returned_rows"] != float64(3) || m["total_rows"] != float64(10) {
		t.Errorf("%v", m)
	}
	if !strings.Contains(m["note"].(string), "3 of 10 rows") {
		t.Errorf("note = %v", m["note"])
	}
}

func TestExactCapIsNotTruncation(t *testing.T) {
	h := newHarness(t)
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, dryRunOK)
	h.fake.reply("GET", "/projects/p/datasets/d/tables/t", 200, `{}`)
	h.runOK(3, 3)
	m, _ := h.callJSON("query", `{"query":"SELECT * FROM p.d.t","max_rows":3}`)
	if m["truncated"] != false || m["returned_rows"] != float64(3) {
		t.Errorf("three rows with max_rows 3 and total 3 is complete: %v", m)
	}
}

func TestTruncationByMaxBytes(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxBytes = envelopeReserve + 80 // room for roughly two rows of ~25 bytes
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, dryRunOK)
	h.fake.reply("GET", "/projects/p/datasets/d/tables/t", 200, `{}`)
	h.runOK(10, 10)
	m, isErr := h.callJSON("query", `{"query":"SELECT * FROM p.d.t"}`)
	if isErr {
		t.Fatalf("%v", m)
	}
	if m["truncated"] != true || m["truncated_by"] != "max_bytes" {
		t.Errorf("%v", m)
	}
	n := int(m["returned_rows"].(float64))
	if n < 1 || n >= 10 {
		t.Errorf("returned %d rows", n)
	}
}

func TestMaxRowsCeilingAndUnknownArgument(t *testing.T) {
	h := newHarness(t)
	m := h.expectError("query", `{"query":"SELECT 1","max_rows":999999}`, toolerr.CodeInvalidArguments)
	if m["details"].(map[string]any)["hard_max_rows"] != float64(config.DefaultHardMaxRows) {
		t.Errorf("%v", m)
	}
	h.expectError("query", `{"query":"SELECT 1","max_row":5}`, toolerr.CodeInvalidArguments)
	h.expectError("query", `{}`, toolerr.CodeInvalidArguments)
	h.expectError("dry_run", `{"query":"SELECT 1","max_rows":5}`, toolerr.CodeInvalidArguments)
	if len(h.fake.calls) != 0 {
		t.Errorf("argument errors must not reach BigQuery: %v", h.fake.calls)
	}
}

func TestDryRunVerdictIsDataNotError(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxBytesBilled = config.MinBytesBilled
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 200, strings.Replace(dryRunOK, `"totalBytesProcessed":"1000"`, `"totalBytesProcessed":"20971520"`, 1))
	h.fake.reply("GET", "/projects/p/datasets/d/tables/t", 200, `{"tableReference":{"projectId":"p","datasetId":"d","tableId":"t"},"numBytes":"20971520","timePartitioning":{"type":"DAY","field":"event_date"}}`)
	m, isErr := h.callJSON("dry_run", `{"query":"SELECT * FROM p.d.t"}`)
	if isErr {
		t.Fatalf("dry_run reports the verdict as data: %v", m)
	}
	if m["allowed"] != false || m["denied_by"] != toolerr.CodeBudgetExceeded || m["within_budget"] != false || m["statement_type"] != "SELECT" {
		t.Errorf("%v", m)
	}
	if rt := m["referenced_tables"].([]any); len(rt) != 1 || rt[0] != "p.d.t" {
		t.Errorf("%v", m["referenced_tables"])
	}
	if len(m["warnings"].([]any)) != 1 {
		t.Errorf("expected warning")
	}
	if h.fake.count("INSERT /projects/billing/jobs") != 0 {
		t.Errorf("dry_run must not run the query")
	}
}

func TestBigQueryErrorsPassThroughContract(t *testing.T) {
	h := newHarness(t)
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 403, `{"error":{"code":403,"message":"Access Denied: Table p:d.t","errors":[{"reason":"accessDenied"}]}}`)
	m := h.expectError("query", `{"query":"SELECT 1 FROM p.d.t"}`, toolerr.CodeAccessDenied)
	if m["retryable"] != false || m["details"].(map[string]any)["reason"] != "accessDenied" {
		t.Errorf("%v", m)
	}
	h.fake.reply("DRYRUN", "/projects/billing/jobs", 400, `{"error":{"code":400,"message":"Syntax error: Unexpected end of script at [1:7]","errors":[{"reason":"invalidQuery","location":"query"}]}}`)
	m = h.expectError("dry_run", `{"query":"SELECT"}`, toolerr.CodeInvalidQuery)
	if !strings.Contains(m["message"].(string), "[1:7]") {
		t.Errorf("message should carry BigQuery's text: %v", m["message"])
	}
}

func TestParamsAreForwardedNamed(t *testing.T) {
	h := newHarness(t)
	var body map[string]any
	h.fake.handlers["DRYRUN /projects/billing/jobs"] = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(dryRunOK))
	}
	h.fake.reply("GET", "/projects/p/datasets/d/tables/t", 200, `{}`)
	h.runOK(1, 1)
	if _, isErr := h.callJSON("query", `{"query":"SELECT @n, @s, @b, @f","params":{"n":3,"s":"x","b":true,"f":1.5}}`); isErr {
		t.Fatal("unexpected error")
	}
	types := map[string]string{}
	extract := func() {
		q := body["configuration"].(map[string]any)["query"].(map[string]any)
		if q["parameterMode"] != "NAMED" {
			t.Errorf("parameterMode = %v", q["parameterMode"])
		}
		types = map[string]string{}
		for _, p := range q["queryParameters"].([]any) {
			pm := p.(map[string]any)
			types[pm["name"].(string)] = pm["parameterType"].(map[string]any)["type"].(string)
		}
	}
	extract()
	want := map[string]string{"n": "INT64", "s": "STRING", "b": "BOOL", "f": "FLOAT64"}
	for k, v := range want {
		if types[k] != v {
			t.Errorf("param %s type = %q, want %q", k, types[k], v)
		}
	}
	h.expectError("query", `{"query":"SELECT @a","params":{"a":[1,2]}}`, toolerr.CodeInvalidArguments)
	h.expectError("query", `{"query":"SELECT @a","params":{"a":{"type":"ARRAY","value":"x"}}}`, toolerr.CodeInvalidArguments)
	if _, isErr := h.callJSON("query", `{"query":"SELECT @d","params":{"d":{"type":"date","value":"2026-09-05"}}}`); isErr {
		t.Fatal("explicit typed parameter should be accepted")
	}
	extract()
	if types["d"] != "DATE" {
		t.Errorf("explicit type not forwarded: %v", types)
	}
}

func TestDescribeTable(t *testing.T) {
	h := newHarness(t)
	h.fake.reply("GET", "/projects/billing/datasets/d/tables/t", 200, `{"tableReference":{"projectId":"billing","datasetId":"d","tableId":"t"},"type":"TABLE","numRows":"42","numBytes":"2048",
		"schema":{"fields":[{"name":"a","type":"STRING","mode":"NULLABLE"},{"name":"r","type":"RECORD","fields":[{"name":"x","type":"INTEGER"}]}]},
		"timePartitioning":{"type":"DAY","field":"a","expirationMs":"34560000000","requirePartitionFilter":true},"clustering":{"fields":["a"]},
		"creationTime":"1788617399000","lastModifiedTime":"1788617399000","location":"US"}`)
	m, isErr := h.callJSON("describe_table", `{"dataset":"d","table":"t"}`)
	if isErr {
		t.Fatalf("%v", m)
	}
	p := m["partition"].(map[string]any)
	if p["column"] != "a" || p["kind"] != "time" || p["granularity"] != "DAY" || p["expiration_days"] != float64(400) || p["require_filter"] != true {
		t.Errorf("partition = %v", p)
	}
	if m["num_rows"] != float64(42) || m["size"] != "2.0 KiB" || m["created"] != "2026-09-05T14:09:59Z" {
		t.Errorf("%v", m)
	}
	sch := m["schema"].([]any)
	if len(sch) != 2 || len(sch[1].(map[string]any)["fields"].([]any)) != 1 {
		t.Errorf("schema = %v", sch)
	}
	h.expectError("describe_table", `{"dataset":"d"}`, toolerr.CodeInvalidArguments)
}

func TestListDatasetsAndTables(t *testing.T) {
	h := newHarness(t)
	h.fake.reply("GET", "/projects/billing/datasets", 200, `{"datasets":[{"datasetReference":{"projectId":"billing","datasetId":"a"},"location":"US"},{"datasetReference":{"projectId":"billing","datasetId":"b"}}]}`)
	m, _ := h.callJSON("list_datasets", `{}`)
	if m["count"] != float64(2) || m["project"] != "billing" {
		t.Errorf("%v", m)
	}
	h.fake.reply("GET", "/projects/other/datasets/a/tables", 200, `{"tables":[{"tableReference":{"projectId":"other","datasetId":"a","tableId":"t"},"type":"TABLE","timePartitioning":{"type":"DAY"}}]}`)
	m, _ = h.callJSON("list_tables", `{"dataset":"a","project":"other"}`)
	ts := m["tables"].([]any)
	if m["count"] != float64(1) || ts[0].(map[string]any)["partition_column"] != "_PARTITIONTIME" {
		t.Errorf("%v", m)
	}
	h.fake.reply("GET", "/projects/billing/datasets/none/tables", 404, `{"error":{"code":404,"message":"Not found: Dataset billing:none","errors":[{"reason":"notFound"}]}}`)
	h.expectError("list_tables", `{"dataset":"none"}`, toolerr.CodeNotFound)
}

func TestGetUsageDocument(t *testing.T) {
	h := newHarness(t)
	h.cfg.Datasets = []string{"p.d"}
	text, isErr := h.call("get_usage", `{}`)
	if isErr {
		t.Fatal("get_usage errored")
	}
	for _, want := range []string{"billing", "p.d", "retryable", "max_rows", "10.0 GiB"} {
		if !strings.Contains(text, want) {
			t.Errorf("usage document lacks %q", want)
		}
	}
	for _, c := range toolerr.Codes {
		if !strings.Contains(text, c.Code) {
			t.Errorf("usage document lacks documented code %q", c.Code)
		}
	}
}

func TestBilledEstimate(t *testing.T) {
	one := &bq.DryRunResult{TotalBytesProcessed: 1000, ReferencedTables: []bq.TableRef{{}}}
	if got := billedEstimate(one); got != config.MinBytesBilled {
		t.Errorf("small scans bill the 10 MiB minimum, got %d", got)
	}
	two := &bq.DryRunResult{TotalBytesProcessed: 1000, ReferencedTables: []bq.TableRef{{}, {}}}
	if got := billedEstimate(two); got != 2*config.MinBytesBilled {
		t.Errorf("two tables bill 20 MiB minimum, got %d", got)
	}
	big := &bq.DryRunResult{TotalBytesProcessed: 3<<30 + 1, ReferencedTables: []bq.TableRef{{}}}
	if got := billedEstimate(big); got != 3<<30+1<<20 {
		t.Errorf("bytes round up to the next MiB, got %d", got)
	}
}

func TestShaperAccounting(t *testing.T) {
	s := newShaper(2, 1<<20)
	if !s.sink(bq.Row{"a": 1}) {
		t.Fatal("first row must be accepted with room to spare")
	}
	if s.sink(bq.Row{"a": 2}) {
		t.Fatal("reaching max_rows returns false")
	}
	if s.truncatedBy != "" || len(s.rows) != 2 {
		t.Errorf("at the cap nothing is truncated yet: %+v", s)
	}
	if s.sink(bq.Row{"a": 3}) || s.truncatedBy != "max_rows" || len(s.rows) != 2 {
		t.Errorf("a row past the cap marks max_rows: %+v", s)
	}
	b := newShaper(100, envelopeReserve+30)
	if !b.sink(bq.Row{"k": "0123456789"}) { // ~20 bytes
		t.Fatal("first row always fits")
	}
	if b.sink(bq.Row{"k": "0123456789"}) || b.truncatedBy != "max_bytes" || len(b.rows) != 1 {
		t.Errorf("second row over the byte budget marks max_bytes: %+v", b)
	}
}
