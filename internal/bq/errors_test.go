package bq

import (
	"testing"

	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

func TestMapHTTPErrorTable(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		code      string
		retryable bool
	}{
		{"invalid query", 400, `{"error":{"code":400,"message":"Unrecognized name: emial at [1:8]","errors":[{"message":"Unrecognized name: emial at [1:8]","domain":"global","reason":"invalidQuery","location":"q","locationType":"parameter"}],"status":"INVALID_ARGUMENT"}}`, toolerr.CodeInvalidQuery, false},
		{"access denied", 403, `{"error":{"code":403,"message":"Access Denied: Table p:d.t","errors":[{"reason":"accessDenied"}],"status":"PERMISSION_DENIED"}}`, toolerr.CodeAccessDenied, false},
		{"not found", 404, `{"error":{"code":404,"message":"Not found: Dataset p:d","errors":[{"reason":"notFound"}]}}`, toolerr.CodeNotFound, false},
		{"rate limit", 403, `{"error":{"code":403,"message":"Exceeded rate limits: too many api requests per user per method","errors":[{"reason":"rateLimitExceeded"}]}}`, toolerr.CodeRateLimited, true},
		{"daily quota", 403, `{"error":{"code":403,"message":"Quota exceeded: Your project exceeded quota for free query bytes scanned per day","errors":[{"reason":"quotaExceeded"}]}}`, toolerr.CodeRateLimited, false},
		{"minute quota", 403, `{"error":{"code":403,"message":"Quota exceeded: concurrent queries","errors":[{"reason":"quotaExceeded"}]}}`, toolerr.CodeRateLimited, true},
		{"backend", 500, `{"error":{"code":500,"message":"An internal error occurred","errors":[{"reason":"backendError"}]}}`, toolerr.CodeBackendError, true},
		{"503 no body", 503, ``, toolerr.CodeBackendError, true},
		{"429 no reason", 429, `{"error":{"code":429,"message":"slow down"}}`, toolerr.CodeRateLimited, true},
		{"401", 401, `{"error":{"code":401,"message":"Request had invalid authentication credentials","status":"UNAUTHENTICATED"}}`, toolerr.CodeAuthError, false},
		{"bytes billed cap", 400, `{"error":{"code":400,"message":"Query exceeded limit for bytes billed: 10737418240","errors":[{"reason":"bytesBilledLimitExceeded"}]}}`, toolerr.CodeBudgetExceeded, false},
		{"resources exceeded", 400, `{"error":{"code":400,"message":"Resources exceeded during query execution","errors":[{"reason":"resourcesExceeded"}]}}`, toolerr.CodeInvalidQuery, false},
		{"timeout reason", 400, `{"error":{"code":400,"message":"Job execution was cancelled: Job timed out","errors":[{"reason":"timeout"}]}}`, toolerr.CodeTimeout, false},
		{"billing not enabled", 403, `{"error":{"code":403,"message":"Billing has not been enabled","errors":[{"reason":"billingNotEnabled"}]}}`, toolerr.CodeAccessDenied, false},
		{"plain 400", 400, `{"error":{"code":400,"message":"Syntax error"}}`, toolerr.CodeInvalidQuery, false},
		{"unknown 418", 418, `teapot`, toolerr.CodeUpstreamError, false},
		{"501 not retryable", 501, `{"error":{"code":501,"message":"not implemented","errors":[{"reason":"notImplemented"}]}}`, toolerr.CodeUpstreamError, false},
		{"api not enabled", 403, `{"error":{"code":403,"message":"BigQuery API has not been used in project 1 before","errors":[{"reason":"accessNotConfigured"}]}}`, toolerr.CodeAccessDenied, false},
		{"duplicate job", 409, `{"error":{"code":409,"message":"Already Exists: Job p:US.bqmcp-1","errors":[{"reason":"duplicate"}]}}`, toolerr.CodeDuplicate, false},
		{"html body", 502, `<html>bad gateway</html>`, toolerr.CodeBackendError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mapHTTPError(tc.status, []byte(tc.body))
			if e.Code != tc.code || e.Retryable != tc.retryable {
				t.Errorf("got %s retryable=%v, want %s retryable=%v (msg %q)", e.Code, e.Retryable, tc.code, tc.retryable, e.Message)
			}
			if e.Details["http_status"] != tc.status {
				t.Errorf("details.http_status = %v", e.Details["http_status"])
			}
			if e.Message == "" {
				t.Errorf("message must not be empty")
			}
		})
	}
}

func TestMapHTTPErrorDetailsCarryLocationAndReason(t *testing.T) {
	e := mapHTTPError(400, []byte(`{"error":{"message":"x","errors":[{"reason":"invalidQuery","location":"query"}],"status":"INVALID_ARGUMENT"}}`))
	if e.Details["reason"] != "invalidQuery" || e.Details["location"] != "query" || e.Details["status"] != "INVALID_ARGUMENT" {
		t.Errorf("details = %v", e.Details)
	}
}

func TestMapJobError(t *testing.T) {
	e := mapJobError(&ErrorProto{Reason: "invalidQuery", Message: "bad", Location: "q"}, "job-1", "US")
	if e.Code != toolerr.CodeInvalidQuery || e.Details["job_id"] != "job-1" || e.Details["job_location"] != "US" || e.Details["location"] != "q" {
		t.Errorf("%+v", e)
	}
	e = mapJobError(&ErrorProto{Reason: "stopped", Message: "Job execution was cancelled: User requested cancellation"}, "", "")
	if e.Code != toolerr.CodeCancelled {
		t.Errorf("an outside cancel maps to cancelled, got %s", e.Code)
	}
	e = mapJobError(&ErrorProto{Reason: "stopped", Message: "Job execution was cancelled: Job timed out after 180.0 sec"}, "", "")
	if e.Code != toolerr.CodeTimeout {
		t.Errorf("a stop for timeout maps to timeout, got %s", e.Code)
	}
	if _, ok := e.Details["job_id"]; ok {
		t.Errorf("empty job id must not appear in details")
	}
}

func TestHintsMatchTheReason(t *testing.T) {
	if e := mapHTTPError(403, []byte(`{"error":{"message":"x","errors":[{"reason":"accessNotConfigured"}]}}`)); !contains(e.Message, "API is not enabled") || contains(e.Message, "jobUser") {
		t.Errorf("accessNotConfigured must not get the IAM hint: %q", e.Message)
	}
	if e := mapHTTPError(403, []byte(`{"error":{"message":"x","errors":[{"reason":"billingNotEnabled"}]}}`)); !contains(e.Message, "billing") || contains(e.Message, "jobUser") {
		t.Errorf("billingNotEnabled must get the billing hint: %q", e.Message)
	}
}

func TestAccessAndAuthHints(t *testing.T) {
	if e := mapHTTPError(403, []byte(`{"error":{"message":"denied","errors":[{"reason":"accessDenied"}]}}`)); !contains(e.Message, "roles/bigquery.jobUser") {
		t.Errorf("access_denied should name the usual missing role: %q", e.Message)
	}
	if e := mapHTTPError(401, nil); !contains(e.Message, "gcloud auth application-default login") {
		t.Errorf("auth_error should name the fix: %q", e.Message)
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
