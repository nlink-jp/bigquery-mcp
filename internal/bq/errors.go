package bq

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

// mapHTTPError turns a non-2xx BigQuery response into the error contract
// (ADR-0004). status is the HTTP status, body the raw response.
func mapHTTPError(status int, body []byte) *toolerr.Error {
	var env apiErrorBody
	_ = json.Unmarshal(body, &env)
	var first ErrorProto
	if len(env.Error.Errors) > 0 {
		first = env.Error.Errors[0]
	}
	msg := env.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		if msg == "" {
			msg = http.StatusText(status)
		}
	}
	e := mapReason(status, first.Reason, msg)
	d := map[string]any{"http_status": status}
	if first.Reason != "" {
		d["reason"] = first.Reason
	}
	if first.Location != "" {
		d["location"] = first.Location
	}
	if env.Error.Status != "" {
		d["status"] = env.Error.Status
	}
	return e.WithDetails(d)
}

// mapJobError turns an error reported inside a 2xx response (a job that
// ran and failed, or a dry run that rejected the statement) into the
// contract.
func mapJobError(ep *ErrorProto, jobID, location string) *toolerr.Error {
	e := mapReason(0, ep.Reason, ep.Message)
	d := map[string]any{}
	if ep.Reason != "" {
		d["reason"] = ep.Reason
	}
	if ep.Location != "" {
		d["location"] = ep.Location
	}
	if jobID != "" {
		d["job_id"] = jobID
	}
	if location != "" {
		d["job_location"] = location
	}
	return e.WithDetails(d)
}

// mapReason is the single table from BigQuery reason + HTTP status to
// {code, retryable}. Unknown reasons fall back on the HTTP status class.
func mapReason(status int, reason, msg string) *toolerr.Error {
	lower := strings.ToLower(msg)
	switch reason {
	case "invalidQuery", "invalid", "resourcesExceeded", "responseTooLarge", "invalidQueryParameter":
		return toolerr.New(toolerr.CodeInvalidQuery, msg)
	case "accessDenied", "billingNotEnabled", "userNotAuthorized":
		return toolerr.New(toolerr.CodeAccessDenied, accessHint(msg))
	case "notFound", "tableNotFound", "datasetNotFound":
		return toolerr.New(toolerr.CodeNotFound, msg)
	case "rateLimitExceeded", "quotaExceeded", "concurrentQueryLimitExceeded":
		e := toolerr.New(toolerr.CodeRateLimited, msg)
		if strings.Contains(lower, "per day") || strings.Contains(lower, "daily") {
			return e // exhausted for the day: a retry cannot help
		}
		return e.AsRetryable()
	case "backendError", "internalError", "jobBackendError", "jobInternalError", "unavailable":
		return toolerr.New(toolerr.CodeBackendError, msg).AsRetryable()
	case "timeout", "stopped", "jobTimeout":
		return toolerr.New(toolerr.CodeTimeout, msg)
	case "bytesBilledLimitExceeded":
		return toolerr.New(toolerr.CodeBudgetExceeded, msg)
	case "authError", "unauthorized":
		return toolerr.New(toolerr.CodeAuthError, authHint(msg))
	}
	switch {
	case status == http.StatusUnauthorized:
		return toolerr.New(toolerr.CodeAuthError, authHint(msg))
	case status == http.StatusForbidden:
		return toolerr.New(toolerr.CodeAccessDenied, accessHint(msg))
	case status == http.StatusNotFound:
		return toolerr.New(toolerr.CodeNotFound, msg)
	case status == http.StatusTooManyRequests:
		return toolerr.New(toolerr.CodeRateLimited, msg).AsRetryable()
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return toolerr.New(toolerr.CodeTimeout, msg)
	case status >= 500:
		return toolerr.New(toolerr.CodeBackendError, msg).AsRetryable()
	case status == http.StatusBadRequest:
		// A 400 without a known reason is still something about the request.
		return toolerr.New(toolerr.CodeInvalidQuery, msg)
	}
	return toolerr.Newf(toolerr.CodeUpstreamError, "BigQuery answered HTTP %d: %s", status, msg)
}

func accessHint(msg string) string {
	return msg + " — the caller usually lacks roles/bigquery.jobUser on the billing project or roles/bigquery.dataViewer on the data; see get_usage"
}

func authHint(msg string) string {
	return msg + " — no usable Application Default Credentials; run `gcloud auth application-default login` and retry"
}

// authError wraps a token-source failure.
func authError(err error) *toolerr.Error {
	return toolerr.New(toolerr.CodeAuthError, authHint(err.Error()))
}

// transportError wraps a network-level failure (DNS, TLS, connection reset).
func transportError(err error) *toolerr.Error {
	return toolerr.Newf(toolerr.CodeUpstreamError, "request to BigQuery failed: %v", err).AsRetryable()
}

// asToolError extracts the contract error from err, or wraps err.
func asToolError(err error) *toolerr.Error {
	var te *toolerr.Error
	if errors.As(err, &te) {
		return te
	}
	return toolerr.New(toolerr.CodeUpstreamError, fmt.Sprint(err))
}
