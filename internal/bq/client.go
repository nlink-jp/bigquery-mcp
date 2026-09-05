package bq

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/nlink-jp/bigquery-mcp/internal/config"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

const (
	// DefaultBase is the BigQuery REST v2 endpoint.
	DefaultBase = "https://bigquery.googleapis.com/bigquery/v2"
	// Scope is requested from the token source (narrows service-account
	// tokens; user ADC keeps the scopes granted at login — ADR-0001).
	Scope = "https://www.googleapis.com/auth/bigquery"
	// Label attached to every job this server runs, for billing attribution.
	Label = "bigquery-mcp"

	// pollWait is how long one jobs.query / getQueryResults call waits
	// server-side before answering jobComplete=false.
	pollWait = 30 * time.Second
	// pollGrace is added to the job timeout before the client gives up
	// waiting on a job BigQuery should already have stopped.
	pollGrace = 30 * time.Second
	// maxBodyBytes bounds a response read (a page is at most 20 MB).
	maxBodyBytes = 64 << 20
	// retryMin/retryMax bound the single jittered retry (ADR-0004).
	retryMin = 500 * time.Millisecond
	retryMax = 1500 * time.Millisecond
)

// Client talks to BigQuery for one billing project.
type Client struct {
	base      string
	http      *http.Client
	project   string
	location  string
	userAgent string
	logger    *slog.Logger

	tokenOnce sync.Once
	tokenSrc  oauth2.TokenSource
	tokenErr  error
	// resolveTokens is how the token source is obtained on first use;
	// tests replace it (nil means "no Authorization header").
	resolveTokens func(ctx context.Context) (oauth2.TokenSource, error)

	sleep  func(ctx context.Context, d time.Duration) error
	jitter func() time.Duration
	newID  func() string
}

// New builds a client on Application Default Credentials. The credential
// lookup is lazy: a machine without ADC still starts the server, and every
// tool answers auth_error with the fix, which is what doctor and the
// clients need (ADR-0004).
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger, version string) (*Client, error) {
	if cfg == nil || cfg.ProjectID == "" {
		return nil, errors.New("bq: a billing project id is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Client{
		base:      DefaultBase,
		http:      &http.Client{Timeout: pollWait + 60*time.Second},
		project:   cfg.ProjectID,
		location:  cfg.Location,
		userAgent: "bigquery-mcp/" + version,
		logger:    logger,
		resolveTokens: func(ctx context.Context) (oauth2.TokenSource, error) {
			return google.DefaultTokenSource(ctx, Scope)
		},
		sleep:  sleepCtx,
		jitter: jitterDuration,
		newID:  newRequestID,
	}
	return c, nil
}

// NewForTest builds a client against a fake server with no auth, no
// sleeping and deterministic request ids.
func NewForTest(base string, hc *http.Client, project, location string) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	n := 0
	return &Client{
		base: base, http: hc, project: project, location: location, userAgent: "bigquery-mcp/test",
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolveTokens: nil,
		sleep:         func(context.Context, time.Duration) error { return nil },
		jitter:        func() time.Duration { return 0 },
		newID: func() string {
			n++
			return fmt.Sprintf("test-request-%d", n)
		},
	}
}

// Project returns the billing project.
func (c *Client) Project() string { return c.project }

// DryRunResult is what the gate reads.
type DryRunResult struct {
	StatementType       string
	TotalBytesProcessed int64
	ReferencedTables    []TableRef
	Schema              *TableSchema
}

// DryRun estimates the query through jobs.insert with dryRun, which is
// the one path that reports referencedTables beside statementType and the
// byte estimate (ADR-0002). A dry-run insert creates no job.
func (c *Client) DryRun(ctx context.Context, sql string, params []QueryParameter) (*DryRunResult, error) {
	return c.dryRunInsert(ctx, sql, params)
}

func (c *Client) dryRunInsert(ctx context.Context, sql string, params []QueryParameter) (*DryRunResult, error) {
	req := insertJobRequest{
		Configuration: jobConfiguration{
			DryRun: true,
			Labels: map[string]string{Label: "true"},
			Query: jobConfigurationQuery{
				Query:           sql,
				UseLegacySQL:    false,
				QueryParameters: params,
			},
		},
	}
	if len(params) > 0 {
		req.Configuration.Query.ParameterMode = "NAMED"
	}
	if c.location != "" {
		req.JobReference = &jobReference{ProjectID: c.project, Location: c.location}
	}
	var job jobResource
	if err := c.do(ctx, http.MethodPost, "projects/"+url.PathEscape(c.project)+"/jobs", nil, req, &job); err != nil {
		return nil, err
	}
	if job.Status.ErrorResult != nil {
		loc := ""
		if job.JobReference != nil {
			loc = job.JobReference.Location
		}
		return nil, mapJobError(job.Status.ErrorResult, "", loc)
	}
	q := job.Statistics.Query
	return &DryRunResult{
		StatementType:       q.StatementType,
		TotalBytesProcessed: parseInt64(q.TotalBytesProcessed),
		ReferencedTables:    q.ReferencedTables,
		Schema:              q.Schema,
	}, nil
}

// QueryOptions drives one run.
type QueryOptions struct {
	SQL            string
	Params         []QueryParameter
	MaxBytesBilled int64
	JobTimeout     time.Duration
	// PageSize bounds rows per page (BigQuery also caps a page at 20 MB).
	PageSize int
	// Sink receives each decoded row; returning false stops fetching.
	Sink func(Row) bool
}

// QueryResult is the run's metadata; rows went to the sink.
type QueryResult struct {
	JobID          string
	Location       string
	StatementType  string
	Schema         *TableSchema
	TotalRows      int64
	BytesProcessed int64
	BytesBilled    int64
	CacheHit       bool
	SlotMs         int64
	// Stopped is true when the sink asked to stop before the last page.
	Stopped bool
}

// Query runs SQL through jobs.query with the kernel caps set (ADR-0002),
// waits for completion, and pages results into the sink until it says
// stop or the pages run out (ADR-0003). The request carries a requestId so
// the single retry after a transport failure cannot run the query twice.
func (c *Client) Query(ctx context.Context, o QueryOptions) (*QueryResult, error) {
	if o.Sink == nil {
		return nil, errors.New("bq: Query needs a sink")
	}
	pageSize := o.PageSize
	if pageSize <= 0 {
		pageSize = 1000
	}
	req := queryRequest{
		Query:           o.SQL,
		UseLegacySQL:    false,
		Location:        c.location,
		TimeoutMs:       pollWait.Milliseconds(),
		MaxResults:      int64(pageSize),
		Labels:          map[string]string{Label: "true"},
		UseQueryCache:   boolPtr(true),
		RequestID:       c.newID(),
		QueryParameters: o.Params,
		FormatOptions:   &formatOptions{UseInt64Timestamp: true},
	}
	if len(o.Params) > 0 {
		req.ParameterMode = "NAMED"
	}
	if o.MaxBytesBilled > 0 {
		req.MaximumBytesBilled = strconv.FormatInt(o.MaxBytesBilled, 10)
	}
	if o.JobTimeout > 0 {
		req.JobTimeoutMs = strconv.FormatInt(o.JobTimeout.Milliseconds(), 10)
	}
	var resp queryResponse
	if err := c.do(ctx, http.MethodPost, "projects/"+url.PathEscape(c.project)+"/queries", nil, req, &resp); err != nil {
		return nil, err
	}
	res := &QueryResult{}
	if resp.JobReference != nil {
		res.JobID = resp.JobReference.JobID
		res.Location = resp.JobReference.Location
	}
	if res.Location == "" {
		res.Location = resp.Location
	}

	// Wait for completion. BigQuery enforces jobTimeoutMs; the client
	// keeps a deadline slightly beyond it so a job the server failed to
	// stop still ends here with a timeout.
	deadline := time.Now().Add(pollGrace)
	if o.JobTimeout > 0 {
		deadline = deadline.Add(o.JobTimeout)
	}
	for !resp.JobComplete {
		if res.JobID == "" {
			return nil, toolerr.New(toolerr.CodeBackendError, "BigQuery answered jobComplete=false without a job reference").AsRetryable()
		}
		if time.Now().After(deadline) {
			return nil, toolerr.Newf(toolerr.CodeTimeout, "the query did not complete within job_timeout (%s)", o.JobTimeout).
				WithDetails(map[string]any{"job_id": res.JobID, "job_location": res.Location})
		}
		if err := c.getQueryResults(ctx, res.JobID, res.Location, "", pageSize, &resp); err != nil {
			return nil, err
		}
	}
	if len(resp.Errors) > 0 && resp.Schema == nil {
		return nil, mapJobError(&resp.Errors[0], res.JobID, res.Location)
	}
	res.StatementType = resp.StatementType
	res.Schema = resp.Schema
	res.TotalRows = parseInt64(resp.TotalRows)
	res.BytesProcessed = parseInt64(resp.TotalBytesProcessed)
	res.BytesBilled = parseInt64(resp.TotalBytesBilled)
	res.SlotMs = parseInt64(resp.TotalSlotMs)
	if resp.CacheHit != nil {
		res.CacheHit = *resp.CacheHit
	}

	// Page rows into the sink.
	for {
		rows, err := decodeRows(resp.Schema, resp.Rows)
		if err != nil {
			return nil, toolerr.Newf(toolerr.CodeUpstreamError, "decode result rows: %v", err).
				WithDetails(map[string]any{"job_id": res.JobID})
		}
		for _, r := range rows {
			if !o.Sink(r) {
				res.Stopped = true
				return res, nil
			}
		}
		if resp.PageToken == "" {
			return res, nil
		}
		if err := c.getQueryResults(ctx, res.JobID, res.Location, resp.PageToken, pageSize, &resp); err != nil {
			return nil, err
		}
	}
}

func (c *Client) getQueryResults(ctx context.Context, jobID, location, pageToken string, pageSize int, out *queryResponse) error {
	q := url.Values{}
	if location != "" {
		q.Set("location", location)
	}
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	q.Set("maxResults", strconv.Itoa(pageSize))
	q.Set("timeoutMs", strconv.FormatInt(pollWait.Milliseconds(), 10))
	q.Set("formatOptions.useInt64Timestamp", "true")
	*out = queryResponse{}
	return c.do(ctx, http.MethodGet, "projects/"+url.PathEscape(c.project)+"/queries/"+url.PathEscape(jobID), q, nil, out)
}

// ListDatasets lists datasets of project (the billing project when empty).
func (c *Client) ListDatasets(ctx context.Context, project string) ([]Dataset, error) {
	if project == "" {
		project = c.project
	}
	var out []Dataset
	token := ""
	for {
		q := url.Values{"maxResults": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var resp datasetListResponse
		if err := c.do(ctx, http.MethodGet, "projects/"+url.PathEscape(project)+"/datasets", q, nil, &resp); err != nil {
			return nil, err
		}
		for _, d := range resp.Datasets {
			out = append(out, Dataset{ProjectID: d.DatasetReference.ProjectID, DatasetID: d.DatasetReference.DatasetID, Location: d.Location})
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		token = resp.NextPageToken
	}
}

// ListTables lists tables and views of a dataset.
func (c *Client) ListTables(ctx context.Context, project, dataset string) ([]TableSummary, error) {
	if project == "" {
		project = c.project
	}
	var out []TableSummary
	token := ""
	for {
		q := url.Values{"maxResults": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var resp tableListResponse
		if err := c.do(ctx, http.MethodGet, "projects/"+url.PathEscape(project)+"/datasets/"+url.PathEscape(dataset)+"/tables", q, nil, &resp); err != nil {
			return nil, err
		}
		for _, t := range resp.Tables {
			col, kind := partitionColumn(t.TimePartitioning, t.RangePartitioning)
			out = append(out, TableSummary{TableID: t.TableReference.TableID, Type: t.Type, PartitionColumn: col, PartitionType: kind})
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		token = resp.NextPageToken
	}
}

// GetTable fetches a table's metadata and schema.
func (c *Client) GetTable(ctx context.Context, project, dataset, table string) (*TableInfo, error) {
	if project == "" {
		project = c.project
	}
	var info TableInfo
	path := "projects/" + url.PathEscape(project) + "/datasets/" + url.PathEscape(dataset) + "/tables/" + url.PathEscape(table)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// do performs one request with auth, maps failures to the contract, and
// retries exactly once on a retryable failure (ADR-0004). Every call this
// client makes is idempotent as sent — reads, dry runs, and jobs.query
// with a requestId — so the retry never duplicates work.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return toolerr.Newf(toolerr.CodeUpstreamError, "encode request: %v", err)
		}
	}
	err := c.once(ctx, method, path, q, payload, out)
	if err == nil {
		return nil
	}
	te := asToolError(err)
	if !te.Retryable {
		return te
	}
	d := c.jitter()
	c.logger.Warn("bigquery call failed; retrying once", "method", method, "path", path, "code", te.Code, "delay", d)
	if serr := c.sleep(ctx, d); serr != nil {
		return te
	}
	if err2 := c.once(ctx, method, path, q, payload, out); err2 != nil {
		return asToolError(err2)
	}
	return nil
}

func (c *Client) once(ctx context.Context, method, path string, q url.Values, payload []byte, out any) error {
	u := c.base + "/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return toolerr.Newf(toolerr.CodeUpstreamError, "build request: %v", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ts, err := c.tokens(ctx); err != nil {
		return authError(err)
	} else if ts != nil {
		tok, err := ts.Token()
		if err != nil {
			return authError(err)
		}
		tok.SetAuthHeader(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return toolerr.New(toolerr.CodeTimeout, "the call was cancelled: "+ctx.Err().Error())
		}
		return transportError(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return transportError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapHTTPError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return toolerr.Newf(toolerr.CodeUpstreamError, "decode BigQuery response: %v", err)
	}
	return nil
}

func (c *Client) tokens(ctx context.Context) (oauth2.TokenSource, error) {
	if c.resolveTokens == nil {
		return nil, nil
	}
	c.tokenOnce.Do(func() {
		c.tokenSrc, c.tokenErr = c.resolveTokens(context.WithoutCancel(ctx))
	})
	return c.tokenSrc, c.tokenErr
}

// CheckAuth resolves the token source and fetches one token; doctor uses it.
func (c *Client) CheckAuth(ctx context.Context) error {
	ts, err := c.tokens(ctx)
	if err != nil {
		return authError(err)
	}
	if ts == nil {
		return nil
	}
	if _, err := ts.Token(); err != nil {
		return authError(err)
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func jitterDuration() time.Duration {
	span := int64(retryMax - retryMin)
	n, err := rand.Int(rand.Reader, big.NewInt(span))
	if err != nil {
		return retryMin
	}
	return retryMin + time.Duration(n.Int64())
}

// newRequestID returns a UUID-shaped 36-character id for jobs.query idempotency.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
