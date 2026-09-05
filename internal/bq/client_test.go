package bq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

// fake is a scriptable BigQuery: handlers keyed by "METHOD path".
type fake struct {
	t        *testing.T
	handlers map[string]http.HandlerFunc
	calls    []string
	bodies   []map[string]any
}

func newFake(t *testing.T) (*fake, *Client) {
	f := &fake{t: t, handlers: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewForTest(srv.URL, srv.Client(), "billing", "")
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key+"?"+r.URL.RawQuery)
	if r.Body != nil {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.bodies = append(f.bodies, m)
	}
	if r.Header.Get("Authorization") != "" {
		f.t.Errorf("test client must not send Authorization")
	}
	if h, ok := f.handlers[key]; ok {
		h(w, r)
		return
	}
	http.Error(w, `{"error":{"code":404,"message":"no handler for `+key+`","errors":[{"reason":"notFound"}]}}`, 404)
}

func (f *fake) on(method, path string, status int, body string) {
	f.handlers[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestDryRunViaInsertCarriesReferencedTables(t *testing.T) {
	f, c := newFake(t)
	f.on("POST", "/projects/billing/jobs", 200, `{"jobReference":{"projectId":"billing","jobId":"j1","location":"US"},"status":{"state":"DONE"},
		"statistics":{"query":{"statementType":"SELECT","totalBytesProcessed":"99","referencedTables":[{"projectId":"p","datasetId":"d","tableId":"t"}]}}}`)
	res, err := c.DryRun(context.Background(), "SELECT * FROM p.d.t", []QueryParameter{{Name: "n", ParameterType: QueryParameterType{Type: "INT64"}, ParameterValue: QueryParameterValue{Value: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ReferencedTables) != 1 || res.ReferencedTables[0].String() != "p.d.t" || res.TotalBytesProcessed != 99 {
		t.Errorf("%+v", res)
	}
	cfg := f.bodies[0]["configuration"].(map[string]any)
	if cfg["dryRun"] != true {
		t.Errorf("insert body must set configuration.dryRun: %v", cfg)
	}
	q := cfg["query"].(map[string]any)
	if q["parameterMode"] != "NAMED" || q["useLegacySql"] != false {
		t.Errorf("query config: %v", q)
	}
}

func TestDryRunInsertErrorResult(t *testing.T) {
	f, c := newFake(t)
	f.on("POST", "/projects/billing/jobs", 200, `{"jobReference":{"jobId":"j1","location":"EU"},"status":{"state":"DONE","errorResult":{"reason":"invalidQuery","message":"Unrecognized name: x","location":"query"}}}`)
	_, err := c.DryRun(context.Background(), "SELECT x", nil)
	te := asToolError(err)
	if te.Code != toolerr.CodeInvalidQuery || te.Details["job_location"] != "EU" {
		t.Errorf("%+v", te)
	}
}

func TestQueryPagesAndStops(t *testing.T) {
	f, c := newFake(t)
	f.on("POST", "/projects/billing/queries", 200, `{"jobReference":{"projectId":"billing","jobId":"j1","location":"US"},"jobComplete":true,
		"schema":{"fields":[{"name":"n","type":"INTEGER"}]},"rows":[{"f":[{"v":"1"}]},{"f":[{"v":"2"}]}],"totalRows":"5","pageToken":"p2",
		"totalBytesProcessed":"100","totalBytesBilled":"10485760","cacheHit":false,"totalSlotMs":"7","statementType":"SELECT"}`)
	page := 0
	f.handlers["GET /projects/billing/queries/j1"] = func(w http.ResponseWriter, r *http.Request) {
		page++
		if r.URL.Query().Get("location") != "US" || r.URL.Query().Get("formatOptions.useInt64Timestamp") != "true" {
			t.Errorf("getQueryResults query: %s", r.URL.RawQuery)
		}
		switch r.URL.Query().Get("pageToken") {
		case "p2":
			_, _ = w.Write([]byte(`{"jobComplete":true,"schema":{"fields":[{"name":"n","type":"INTEGER"}]},"rows":[{"f":[{"v":"3"}]},{"f":[{"v":"4"}]}],"totalRows":"5","pageToken":"p3"}`))
		case "p3":
			_, _ = w.Write([]byte(`{"jobComplete":true,"schema":{"fields":[{"name":"n","type":"INTEGER"}]},"rows":[{"f":[{"v":"5"}]}],"totalRows":"5"}`))
		default:
			t.Errorf("unexpected page token %q", r.URL.Query().Get("pageToken"))
		}
	}
	var got []int64
	res, err := c.Query(context.Background(), QueryOptions{SQL: "SELECT n", MaxBytesBilled: 1 << 30, JobTimeout: time.Minute, PageSize: 2, Sink: func(r Row) bool {
		got = append(got, r["n"].(int64))
		return len(got) < 3 // stop after three rows
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped || len(got) != 3 || got[2] != 3 || res.TotalRows != 5 || res.BytesBilled != 10485760 || res.JobID != "j1" || res.Location != "US" || res.SlotMs != 7 {
		t.Errorf("got %v res %+v", got, res)
	}
	if page != 1 {
		t.Errorf("sink stop must end paging; fetched %d extra pages", page)
	}
	b := f.bodies[0]
	if b["maximumBytesBilled"] != "1073741824" || b["jobTimeoutMs"] != "60000" || b["requestId"] != "test-request-1" || b["useQueryCache"] != true {
		t.Errorf("run body: %v", b)
	}
	if b["labels"].(map[string]any)[Label] != "true" {
		t.Errorf("label missing: %v", b["labels"])
	}
	if b["formatOptions"].(map[string]any)["useInt64Timestamp"] != true {
		t.Errorf("useInt64Timestamp missing")
	}
}

func TestQueryWaitsForCompletion(t *testing.T) {
	f, c := newFake(t)
	f.on("POST", "/projects/billing/queries", 200, `{"jobReference":{"jobId":"j2","location":"asia-northeast1"},"jobComplete":false}`)
	polls := 0
	f.handlers["GET /projects/billing/queries/j2"] = func(w http.ResponseWriter, r *http.Request) {
		polls++
		if polls == 1 {
			_, _ = w.Write([]byte(`{"jobComplete":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"jobComplete":true,"schema":{"fields":[{"name":"a","type":"STRING"}]},"rows":[{"f":[{"v":"z"}]}],"totalRows":"1","statementType":"SELECT"}`))
	}
	n := 0
	res, err := c.Query(context.Background(), QueryOptions{SQL: "q", JobTimeout: time.Minute, Sink: func(Row) bool { n++; return true }})
	if err != nil {
		t.Fatal(err)
	}
	if polls != 2 || n != 1 || res.Stopped || res.Location != "asia-northeast1" {
		t.Errorf("polls=%d rows=%d res=%+v", polls, n, res)
	}
}

func TestQueryJobErrorInsideResponse(t *testing.T) {
	f, c := newFake(t)
	f.on("POST", "/projects/billing/queries", 200, `{"jobReference":{"jobId":"j3","location":"US"},"jobComplete":true,"errors":[{"reason":"resourcesExceeded","message":"Resources exceeded during query execution"}]}`)
	_, err := c.Query(context.Background(), QueryOptions{SQL: "q", Sink: func(Row) bool { return true }})
	te := asToolError(err)
	if te.Code != toolerr.CodeInvalidQuery || te.Details["job_id"] != "j3" {
		t.Errorf("%+v", te)
	}
}

func TestRetryOnceOnRetryable(t *testing.T) {
	f, c := newFake(t)
	var n int32
	f.handlers["GET /projects/billing/datasets"] = func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			http.Error(w, `{"error":{"code":503,"message":"try later","errors":[{"reason":"backendError"}]}}`, 503)
			return
		}
		_, _ = w.Write([]byte(`{"datasets":[{"datasetReference":{"projectId":"billing","datasetId":"ds"},"location":"US"}]}`))
	}
	ds, err := c.ListDatasets(context.Background(), "")
	if err != nil || len(ds) != 1 || ds[0].DatasetID != "ds" {
		t.Fatalf("ds=%v err=%v", ds, err)
	}
	if n != 2 {
		t.Errorf("expected exactly one retry, got %d calls", n)
	}
	// Two failures: the error comes back retryable and no third call is made.
	atomic.StoreInt32(&n, 0)
	f.on("GET", "/projects/billing/datasets", 503, `{"error":{"code":503,"message":"still down","errors":[{"reason":"backendError"}]}}`)
	_, err = c.ListDatasets(context.Background(), "")
	te := asToolError(err)
	if te.Code != toolerr.CodeBackendError || !te.Retryable {
		t.Errorf("%+v", te)
	}
	if len(f.calls) != 4 {
		t.Errorf("expected 2 + 2 calls, got %d: %v", len(f.calls), f.calls)
	}
}

func TestNoRetryOnNonRetryable(t *testing.T) {
	f, c := newFake(t)
	f.on("POST", "/projects/billing/jobs", 400, `{"error":{"code":400,"message":"Syntax error","errors":[{"reason":"invalidQuery"}]}}`)
	_, err := c.DryRun(context.Background(), "SELEC", nil)
	te := asToolError(err)
	if te.Code != toolerr.CodeInvalidQuery || te.Retryable {
		t.Errorf("%+v", te)
	}
	if len(f.calls) != 1 {
		t.Errorf("non-retryable error must not be retried: %v", f.calls)
	}
}

func TestListTablesAndGetTable(t *testing.T) {
	f, c := newFake(t)
	f.on("GET", "/projects/other/datasets/d/tables", 200, `{"tables":[
		{"tableReference":{"projectId":"other","datasetId":"d","tableId":"t1"},"type":"TABLE","timePartitioning":{"type":"DAY","field":"event_date"}},
		{"tableReference":{"projectId":"other","datasetId":"d","tableId":"v1"},"type":"VIEW"},
		{"tableReference":{"projectId":"other","datasetId":"d","tableId":"t2"},"type":"TABLE","timePartitioning":{"type":"DAY"}}]}`)
	ts, err := c.ListTables(context.Background(), "other", "d")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 3 || ts[0].PartitionColumn != "event_date" || ts[1].PartitionColumn != "" || ts[2].PartitionColumn != "_PARTITIONTIME" {
		t.Errorf("%+v", ts)
	}
	f.on("GET", "/projects/billing/datasets/d/tables/t1", 200, `{"tableReference":{"projectId":"billing","datasetId":"d","tableId":"t1"},"type":"TABLE","numRows":"12","numBytes":"3456",
		"schema":{"fields":[{"name":"a","type":"STRING","mode":"NULLABLE"}]},"timePartitioning":{"type":"DAY","field":"a","expirationMs":"34560000000"},"clustering":{"fields":["a"]},"creationTime":"1","lastModifiedTime":"2"}`)
	info, err := c.GetTable(context.Background(), "", "d", "t1")
	if err != nil {
		t.Fatal(err)
	}
	col, kind := info.PartitionColumn()
	if col != "a" || kind != "time" || info.NumRows != "12" || info.Clustering.Fields[0] != "a" {
		t.Errorf("%+v", info)
	}
	if !strings.Contains(f.calls[len(f.calls)-1], "/projects/billing/datasets/d/tables/t1") {
		t.Errorf("empty project must resolve to the billing project: %v", f.calls)
	}
}

func TestRequestIDsAreUUIDShaped(t *testing.T) {
	id := newRequestID()
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("bad request id %q", id)
	}
	if newRequestID() == id {
		t.Errorf("ids must be unique")
	}
}
