// Package toolerr defines the structured tool error every bigquery-mcp tool
// returns to its client. Each error carries a stable code (a slug an agent
// can branch on), a human-readable message, whether a retry can help, and
// machine-readable details that say what would make the call pass.
//
// The type satisfies the error interface, and its Is method compares by
// Code so errors.Is works with sentinel values regardless of the Message.
//
// Ported from nlink-jp/data-toolbox-mcp with the code table swapped and the
// retryable flag added (bigquery-mcp ADR-0004).
package toolerr

import "fmt"

// Error is a structured tool error.
type Error struct {
	// Code is a stable slug for client-side branching (e.g. "budget_exceeded").
	Code string `json:"code"`
	// Message is a human-readable summary that says what to change.
	Message string `json:"message"`
	// Retryable is true when the same call may succeed if repeated: the
	// server has already retried once with jitter before returning it.
	// It is always present in the JSON so an agent never has to guess.
	Retryable bool `json:"retryable"`
	// Details carries machine-readable context (BigQuery reason, location,
	// job id, bytes, budget...).
	Details map[string]any `json:"details,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// Is reports whether target is a *Error with the same Code, so sentinel
// values work under errors.Is regardless of Message and Details.
func (e *Error) Is(target error) bool {
	te, ok := target.(*Error)
	if !ok {
		return false
	}
	return te.Code == e.Code
}

// WithDetails returns a copy of e with the given details attached.
func (e *Error) WithDetails(d map[string]any) *Error {
	cp := *e
	cp.Details = d
	return &cp
}

// AsRetryable returns a copy of e flagged retryable.
func (e *Error) AsRetryable() *Error {
	cp := *e
	cp.Retryable = true
	return &cp
}

// New creates an Error.
func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf creates an Error with a printf-formatted message.
func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Stable error codes. Adding a code is a no-op for older clients (they fall
// back to Message); renaming one is a breaking change. Codes is the single
// documented list: get_usage renders it and the tests check that every code
// the server can produce appears in it (ADR-0004).
const (
	CodeInvalidArguments    = "invalid_arguments"
	CodeInvalidQuery        = "invalid_query"
	CodeStatementNotAllowed = "statement_not_allowed"
	CodeDatasetNotAllowed   = "dataset_not_allowed"
	CodeBudgetExceeded      = "budget_exceeded"
	CodeAccessDenied        = "access_denied"
	CodeNotFound            = "not_found"
	CodeRateLimited         = "rate_limited"
	CodeBackendError        = "backend_error"
	CodeTimeout             = "timeout"
	CodeCancelled           = "cancelled"
	CodeAuthError           = "auth_error"
	CodeDuplicate           = "duplicate"
	CodeUpstreamError       = "upstream_error"
)

// CodeDoc documents one code for operators and agents.
type CodeDoc struct {
	Code      string
	Cause     string
	Recovery  string
	Retryable string // "yes", "no", or a qualified answer
}

// Codes is the documented list, in the order get_usage prints it.
var Codes = []CodeDoc{
	{CodeInvalidQuery, "BigQuery rejected the SQL (details.location points at it), the query is too large, or it needs more resources than a single query may use", "fix the SQL", "no"},
	{CodeStatementNotAllowed, "the dry run classified the statement as something other than SELECT (scripts, DML, DDL, EXPORT, CALL, ASSERT)", "rewrite as one SELECT", "no"},
	{CodeDatasetNotAllowed, "a referenced table or routine is outside [access] datasets", "query the allowed datasets, or ask the operator", "no"},
	{CodeBudgetExceeded, "the dry-run estimate is above [budget] max_bytes_billed (nothing ran), or BigQuery stopped the job at maximumBytesBilled (details.reason says which)", "filter on the partition column, select fewer columns, aggregate; LIMIT does not reduce scanned bytes", "no"},
	{CodeAccessDenied, "missing IAM (roles/bigquery.jobUser on the billing project, roles/bigquery.dataViewer on the data), billing disabled, or the BigQuery API not enabled", "tell the operator which table and role", "no"},
	{CodeNotFound, "project, dataset, table or job does not exist, or is not in the configured location", "check the id with list_*", "no"},
	{CodeRateLimited, "a quota or rate limit", "wait and retry; a per-day quota resets at midnight Pacific and is reported retryable: false", "yes unless the message says per day"},
	{CodeBackendError, "a BigQuery internal error; the server already retried once", "retry later, or report", "yes"},
	{CodeTimeout, "the job exceeded job_timeout, or the call was cancelled by the client", "narrow the query; long jobs are a Phase 2 feature", "no"},
	{CodeCancelled, "the job was stopped from outside (console, bq cancel)", "run it again if that was not intended", "no"},
	{CodeAuthError, "no usable Application Default Credentials", "the operator runs gcloud auth application-default login", "no"},
	{CodeInvalidArguments, "argument type, missing field, unknown field, max_rows above the ceiling", "fix the call", "no"},
	{CodeDuplicate, "a job with this id already exists (the server's own retry handles this internally)", "report it if it reaches you", "no"},
	{CodeUpstreamError, "an HTTP or transport failure that maps to nothing above", "report it", "sometimes"},
}
