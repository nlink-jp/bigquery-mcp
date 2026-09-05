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

// fake is a scriptable BigQuery: handlers keyed by "METHOD path". POSTs to
// /jobs are split by configuration.dryRun into "DRYRUN" and "INSERT".
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
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") {
		if cfg, ok := body["configuration"].(map[string]any); ok && cfg["dryRun"] == true {
			key = "DRYRUN " + r.URL.Path
		} else {
			key = "INSERT " + r.URL.Path
		}
	}
	f.calls = append(f.calls, key+"?"+r.URL.RawQuery)
	if body != nil {
		f.bodies = append(f.bodies, body)
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

func (f *fake) on(key string, status int, body string) {
	f.handlers[key] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func (f *fake) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const jobDone = `{"jobReference":{"projectId":"billing","jobId":"bqmcp-test-1","location":"US"},"status":{"state":"DONE"},
	"statistics":{"totalSlotMs":"7","query":{"statementType":"SELECT","totalBytesProcessed":"100","totalBytesBilled":"10485760","cacheHit":false,"totalSlotMs":"7"}}}`

func TestDryRunCarriesReferencesAccuracyAndLocation(t *testing.T) {
	f, c := newFake(t)
	f.on("DRYRUN /projects/billing/jobs", 200, `{"jobReference":{"projectId":"billing","jobId":"j1","location":"US"},"status":{"state":"DONE"},
		"statistics":{"query":{"statementType":"SELECT","totalBytesProcessed":"99","totalBytesProcessedAccuracy":"PRECISE",
		"referencedTables":[{"projectId":"p","datasetId":"d","tableId":"t"}],"referencedRoutines":[{"projectId":"p","datasetId":"fn","routineId":"tvf"}],
		"undeclaredQueryParameters":[{"name":"day","parameterType":{"type":"DATE"}}]}}}`)
	res, err := c.DryRun(context.Background(), "SELECT * FROM p.d.t", []QueryParameter{{Name: "n", ParameterType: QueryParameterType{Type: "INT64"}, ParameterValue: QueryParameterValue{Value: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ReferencedTables) != 1 || res.ReferencedTables[0].String() != "p.d.t" || res.TotalBytesProcessed != 99 || res.Accuracy != "PRECISE" || res.Location != "US" {
		t.Errorf("%+v", res)
	}
	if len(res.ReferencedRoutines) != 1 || res.ReferencedRoutines[0].DatasetKey() != "p.fn" || len(res.UndeclaredParams) != 1 || res.UndeclaredParams[0].ParameterType.Type != "DATE" {
		t.Errorf("%+v", res)
	}
	cfg := f.bodies[0]["configuration"].(map[string]any)
	q := cfg["query"].(map[string]any)
	if cfg["dryRun"] != true || q["parameterMode"] != "NAMED" || q["useLegacySql"] != false {
		t.Errorf("dry-run body: %v", cfg)
	}
	if _, has := cfg["jobTimeoutMs"]; has {
		t.Errorf("dry run must not carry a job timeout")
	}
}

func TestDryRunErrorResult(t *testing.T) {
	f, c := newFake(t)
	f.on("DRYRUN /projects/billing/jobs", 200, `{"jobReference":{"jobId":"j1","location":"EU"},"status":{"state":"DONE","errorResult":{"reason":"invalidQuery","message":"Unrecognized name: x","location":"query"}}}`)
	_, err := c.DryRun(context.Background(), "SELECT x", nil)
	te := asToolError(err)
	if te.Code != toolerr.CodeInvalidQuery || te.Details["job_location"] != "EU" {
		t.Errorf("%+v", te)
	}
}

func TestQueryInsertsNamedJobPagesAndStops(t *testing.T) {
	f, c := newFake(t)
	f.on("INSERT /projects/billing/jobs", 200, `{"jobReference":{"projectId":"billing","jobId":"bqmcp-test-1","location":"US"},"status":{"state":"RUNNING"}}`)
	page := 0
	f.handlers["GET /projects/billing/queries/bqmcp-test-1"] = func(w http.ResponseWriter, r *http.Request) {
		page++
		q := r.URL.Query()
		if q.Get("location") != "US" || q.Get("formatOptions.timestampOutputFormat") != "ISO8601_STRING" || q.Get("timeoutMs") == "" {
			t.Errorf("getQueryResults query: %s", r.URL.RawQuery)
		}
		switch q.Get("pageToken") {
		case "":
			_, _ = w.Write([]byte(`{"jobReference":{"jobId":"bqmcp-test-1","location":"US"},"jobComplete":true,"schema":{"fields":[{"name":"n","type":"INTEGER"},{"name":"ts","type":"TIMESTAMP"}]},
				"rows":[{"f":[{"v":"1"},{"v":"2026-09-05T14:09:59.123456789012Z"}]},{"f":[{"v":"2"},{"v":null}]}],"totalRows":"5","pageToken":"p2","totalBytesProcessed":"100"}`))
		case "p2":
			_, _ = w.Write([]byte(`{"jobComplete":true,"schema":{"fields":[{"name":"n","type":"INTEGER"},{"name":"ts","type":"TIMESTAMP"}]},"rows":[{"f":[{"v":"3"},{"v":null}]},{"f":[{"v":"4"},{"v":null}]}],"totalRows":"5","pageToken":"p3"}`))
		default:
			t.Errorf("unexpected page token %q", q.Get("pageToken"))
		}
	}
	f.on("GET /projects/billing/jobs/bqmcp-test-1", 200, jobDone)
	var got []int64
	var firstTS any
	res, err := c.Query(context.Background(), QueryOptions{SQL: "SELECT n", MaxBytesBilled: 1 << 30, JobTimeout: time.Minute, PageSize: 2, Sink: func(r Row) bool {
		if firstTS == nil {
			firstTS = r["ts"]
		}
		got = append(got, r["n"].(int64))
		return len(got) < 3 // stop after three rows
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped || len(got) != 3 || got[2] != 3 || res.TotalRows != 5 || res.BytesBilled != 10485760 || res.JobID != "bqmcp-test-1" || res.Location != "US" || res.SlotMs != 7 || res.StatementType != "SELECT" {
		t.Errorf("got %v res %+v", got, res)
	}
	if firstTS != "2026-09-05T14:09:59.123456789012Z" {
		t.Errorf("ISO timestamps pass through untouched, got %v", firstTS)
	}
	if page != 2 {
		t.Errorf("sink stop must end paging; fetched %d pages", page)
	}
	b := f.bodies[0]
	ref := b["jobReference"].(map[string]any)
	if ref["jobId"] != "bqmcp-test-1" || ref["projectId"] != "billing" {
		t.Errorf("job reference: %v", ref)
	}
	cfg := b["configuration"].(map[string]any)
	q := cfg["query"].(map[string]any)
	if q["maximumBytesBilled"] != "1073741824" || cfg["jobTimeoutMs"] != "60000" || q["useQueryCache"] != true || q["priority"] != "INTERACTIVE" {
		t.Errorf("run body: %v", cfg)
	}
	if cfg["labels"].(map[string]any)[Label] != "true" {
		t.Errorf("label missing: %v", cfg["labels"])
	}
	if _, has := cfg["dryRun"]; has && cfg["dryRun"] == true {
		t.Errorf("the run must not be a dry run")
	}
}

func TestQueryWaitsForCompletionAndReadsStatsFromJobsGet(t *testing.T) {
	f, c := newFake(t)
	f.on("INSERT /projects/billing/jobs", 200, `{"jobReference":{"jobId":"bqmcp-test-1","location":"asia-northeast1"},"status":{"state":"PENDING"}}`)
	polls := 0
	f.handlers["GET /projects/billing/queries/bqmcp-test-1"] = func(w http.ResponseWriter, r *http.Request) {
		polls++
		if r.URL.Query().Get("location") != "asia-northeast1" {
			t.Errorf("regional location must be carried: %s", r.URL.RawQuery)
		}
		if polls == 1 {
			_, _ = w.Write([]byte(`{"jobComplete":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"jobComplete":true,"schema":{"fields":[{"name":"a","type":"STRING"}]},"rows":[{"f":[{"v":"z"}]}],"totalRows":"1"}`))
	}
	f.on("GET /projects/billing/jobs/bqmcp-test-1", 200, `{"statistics":{"query":{"statementType":"SELECT","totalBytesBilled":"20971520","cacheHit":true,"totalSlotMs":"11"}}}`)
	n := 0
	res, err := c.Query(context.Background(), QueryOptions{SQL: "q", JobTimeout: time.Minute, Sink: func(Row) bool { n++; return true }})
	if err != nil {
		t.Fatal(err)
	}
	if polls != 2 || n != 1 || res.Stopped || res.BytesBilled != 20971520 || !res.CacheHit || res.SlotMs != 11 {
		t.Errorf("polls=%d rows=%d res=%+v", polls, n, res)
	}
	if !strings.Contains(strings.Join(f.calls, " "), "GET /projects/billing/jobs/bqmcp-test-1?location=asia-northeast1") {
		t.Errorf("jobs.get must carry the location: %v", f.calls)
	}
}

func TestQueryRetryAfterTransportFailureContinuesWithExistingJob(t *testing.T) {
	f, c := newFake(t)
	var inserts int32
	f.handlers["INSERT /projects/billing/jobs"] = func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&inserts, 1) == 1 {
			// The request reached BigQuery but the response was lost.
			http.Error(w, `{"error":{"code":503,"message":"gateway hiccup","errors":[{"reason":"backendError"}]}}`, 503)
			return
		}
		http.Error(w, `{"error":{"code":409,"message":"Already Exists: Job billing:US.bqmcp-test-1","errors":[{"reason":"duplicate"}]}}`, 409)
	}
	f.on("GET /projects/billing/queries/bqmcp-test-1", 200, `{"jobReference":{"jobId":"bqmcp-test-1","location":"US"},"jobComplete":true,"schema":{"fields":[{"name":"a","type":"STRING"}]},"rows":[{"f":[{"v":"z"}]}],"totalRows":"1"}`)
	f.on("GET /projects/billing/jobs/bqmcp-test-1", 200, jobDone)
	n := 0
	res, err := c.Query(context.Background(), QueryOptions{SQL: "q", Sink: func(Row) bool { n++; return true }})
	if err != nil {
		t.Fatalf("a duplicate after retry must continue with the existing job: %v", err)
	}
	if inserts != 2 || n != 1 || res.JobID != "bqmcp-test-1" {
		t.Errorf("inserts=%d rows=%d res=%+v", inserts, n, res)
	}
}

func TestQueryJobErrorResult(t *testing.T) {
	f, c := newFake(t)
	f.on("INSERT /projects/billing/jobs", 200, `{"jobReference":{"jobId":"bqmcp-test-1","location":"US"},"status":{"state":"DONE","errorResult":{"reason":"bytesBilledLimitExceeded","message":"Query exceeded limit for bytes billed: 10485760"}}}`)
	_, err := c.Query(context.Background(), QueryOptions{SQL: "q", Sink: func(Row) bool { return true }})
	te := asToolError(err)
	if te.Code != toolerr.CodeBudgetExceeded || te.Details["job_id"] != "bqmcp-test-1" || te.Retryable {
		t.Errorf("%+v", te)
	}
	if !strings.Contains(te.Message, "maximumBytesBilled") {
		t.Errorf("the kernel-cap hint should distinguish it from the gate: %q", te.Message)
	}
}

func TestQueryErrorInsideResultsPage(t *testing.T) {
	f, c := newFake(t)
	f.on("INSERT /projects/billing/jobs", 200, `{"jobReference":{"jobId":"bqmcp-test-1","location":"US"},"status":{"state":"RUNNING"}}`)
	f.on("GET /projects/billing/queries/bqmcp-test-1", 200, `{"jobComplete":true,"errors":[{"reason":"resourcesExceeded","message":"Resources exceeded during query execution"}]}`)
	_, err := c.Query(context.Background(), QueryOptions{SQL: "q", Sink: func(Row) bool { return true }})
	te := asToolError(err)
	if te.Code != toolerr.CodeInvalidQuery || te.Details["job_id"] != "bqmcp-test-1" {
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
	atomic.StoreInt32(&n, 0)
	f.on("GET /projects/billing/datasets", 503, `{"error":{"code":503,"message":"still down","errors":[{"reason":"backendError"}]}}`)
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
	f.on("DRYRUN /projects/billing/jobs", 400, `{"error":{"code":400,"message":"Syntax error","errors":[{"reason":"invalidQuery"}]}}`)
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
	f.on("GET /projects/other/datasets/d/tables", 200, `{"tables":[
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
	f.on("GET /projects/billing/datasets/d/tables/t1", 200, `{"tableReference":{"projectId":"billing","datasetId":"d","tableId":"t1"},"type":"VIEW","numRows":"12","numBytes":"3456",
		"schema":{"fields":[{"name":"a","type":"STRING","mode":"NULLABLE"}]},"timePartitioning":{"type":"DAY","field":"a","expirationMs":"34560000000"},"clustering":{"fields":["a"]},"creationTime":"1","lastModifiedTime":"2","view":{"query":"SELECT 1"}}`)
	info, err := c.GetTable(context.Background(), "", "d", "t1")
	if err != nil {
		t.Fatal(err)
	}
	col, kind := info.PartitionColumn()
	if col != "a" || kind != "time" || info.NumRows != "12" || info.Clustering.Fields[0] != "a" || info.View == nil || info.View.Query != "SELECT 1" {
		t.Errorf("%+v", info)
	}
	if !strings.Contains(f.calls[len(f.calls)-1], "/projects/billing/datasets/d/tables/t1") {
		t.Errorf("empty project must resolve to the billing project: %v", f.calls)
	}
}

func TestJobIDsAreValidAndUnique(t *testing.T) {
	id := jobIDPrefix + newID()
	if len(id) != len(jobIDPrefix)+32 {
		t.Errorf("bad job id %q", id)
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			t.Errorf("job id %q has a character outside BigQuery's alphabet", id)
		}
	}
	if newID() == newID() {
		t.Errorf("ids must be unique")
	}
}

func TestEveryProducedCodeIsDocumented(t *testing.T) {
	documented := map[string]bool{}
	for _, c := range toolerr.Codes {
		documented[c.Code] = true
	}
	for _, c := range ProducedCodes() {
		if !documented[c] {
			t.Errorf("code %q can be produced but is not in toolerr.Codes", c)
		}
	}
}
