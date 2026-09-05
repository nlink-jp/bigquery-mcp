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
// back to Message); renaming one is a breaking change.
const (
	// CodeInvalidArguments: argument type, missing argument, max_rows above the ceiling.
	CodeInvalidArguments = "invalid_arguments"
	// CodeInvalidQuery: BigQuery rejected the SQL (syntax, unknown column, too long).
	CodeInvalidQuery = "invalid_query"
	// CodeStatementNotAllowed: the dry run classified the statement as something other than SELECT.
	CodeStatementNotAllowed = "statement_not_allowed"
	// CodeDatasetNotAllowed: a referenced table lies outside [access] datasets.
	CodeDatasetNotAllowed = "dataset_not_allowed"
	// CodeBudgetExceeded: the dry run estimate is above [budget] max_bytes_billed; nothing ran.
	CodeBudgetExceeded = "budget_exceeded"
	// CodeAccessDenied: BigQuery accessDenied (a missing IAM role).
	CodeAccessDenied = "access_denied"
	// CodeNotFound: project, dataset, table or job not found.
	CodeNotFound = "not_found"
	// CodeRateLimited: a transient quota or rate limit; retryable.
	CodeRateLimited = "rate_limited"
	// CodeBackendError: BigQuery backendError / internalError; retryable.
	CodeBackendError = "backend_error"
	// CodeTimeout: the job exceeded job_timeout, or the results wait ran out.
	CodeTimeout = "timeout"
	// CodeAuthError: ADC missing, expired or not renewable.
	CodeAuthError = "auth_error"
	// CodeUpstreamError: an HTTP or transport failure that maps to nothing above.
	CodeUpstreamError = "upstream_error"
)
