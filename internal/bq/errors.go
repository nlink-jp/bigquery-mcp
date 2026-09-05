package bq

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

// reasonRule maps one BigQuery error reason to the contract. Reason first,
// HTTP status only as a fallback: BigQuery answers 403 for quota and rate
// limits as well as for missing IAM, so the status alone misleads.
type reasonRule struct {
	code      string
	retryable bool
	hint      func(msg string) string
}

// reasonRules is the single reason table (ADR-0004 §2). Reasons are the
// strings of the BigQuery error table; the Discovery document carries no
// enumeration, so errors_test.go pins each with a recorded body.
var reasonRules = map[string]reasonRule{
	"invalidQuery":                 {toolerr.CodeInvalidQuery, false, nil},
	"invalid":                      {toolerr.CodeInvalidQuery, false, nil},
	"invalidQueryParameter":        {toolerr.CodeInvalidQuery, false, nil},
	"resourcesExceeded":            {toolerr.CodeInvalidQuery, false, resourcesHint},
	"responseTooLarge":             {toolerr.CodeInvalidQuery, false, resourcesHint},
	"billingTierLimitExceeded":     {toolerr.CodeInvalidQuery, false, resourcesHint},
	"bytesBilledLimitExceeded":     {toolerr.CodeBudgetExceeded, false, kernelCapHint},
	"accessDenied":                 {toolerr.CodeAccessDenied, false, accessHint},
	"userNotAuthorized":            {toolerr.CodeAccessDenied, false, accessHint},
	"billingNotEnabled":            {toolerr.CodeAccessDenied, false, billingHint},
	"accessNotConfigured":          {toolerr.CodeAccessDenied, false, apiHint},
	"notFound":                     {toolerr.CodeNotFound, false, nil},
	"tableNotFound":                {toolerr.CodeNotFound, false, nil},
	"datasetNotFound":              {toolerr.CodeNotFound, false, nil},
	"rateLimitExceeded":            {toolerr.CodeRateLimited, true, nil},
	"quotaExceeded":                {toolerr.CodeRateLimited, true, nil},
	"concurrentQueryLimitExceeded": {toolerr.CodeRateLimited, true, nil},
	"backendError":                 {toolerr.CodeBackendError, true, nil},
	"internalError":                {toolerr.CodeBackendError, true, nil},
	"jobBackendError":              {toolerr.CodeBackendError, true, nil},
	"jobInternalError":             {toolerr.CodeBackendError, true, nil},
	"unavailable":                  {toolerr.CodeBackendError, true, nil},
	"timeout":                      {toolerr.CodeTimeout, false, nil},
	"jobTimeout":                   {toolerr.CodeTimeout, false, nil},
	"stopped":                      {toolerr.CodeCancelled, false, nil},
	"duplicate":                    {toolerr.CodeDuplicate, false, nil},
	"authError":                    {toolerr.CodeAuthError, false, authHint},
	"unauthorized":                 {toolerr.CodeAuthError, false, authHint},
	"notImplemented":               {toolerr.CodeUpstreamError, false, nil},
}

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

// mapReason applies reasonRules, then the HTTP status class as a fallback.
func mapReason(status int, reason, msg string) *toolerr.Error {
	if rule, ok := reasonRules[reason]; ok {
		if rule.hint != nil {
			msg = rule.hint(msg)
		}
		e := toolerr.New(rule.code, msg)
		if rule.retryable && !dailyQuota(msg) {
			return e.AsRetryable()
		}
		if reason == "stopped" && strings.Contains(strings.ToLower(msg), "timed out") {
			return toolerr.New(toolerr.CodeTimeout, msg)
		}
		return e
	}
	switch {
	case status == http.StatusUnauthorized:
		return toolerr.New(toolerr.CodeAuthError, authHint(msg))
	case status == http.StatusForbidden:
		return toolerr.New(toolerr.CodeAccessDenied, accessHint(msg))
	case status == http.StatusNotFound:
		return toolerr.New(toolerr.CodeNotFound, msg)
	case status == http.StatusConflict:
		return toolerr.New(toolerr.CodeDuplicate, msg)
	case status == http.StatusTooManyRequests:
		return toolerr.New(toolerr.CodeRateLimited, msg).AsRetryable()
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return toolerr.New(toolerr.CodeTimeout, msg)
	case status == http.StatusNotImplemented:
		return toolerr.Newf(toolerr.CodeUpstreamError, "BigQuery answered HTTP 501: %s", msg)
	case status >= 500:
		return toolerr.New(toolerr.CodeBackendError, msg).AsRetryable()
	case status == http.StatusBadRequest:
		// A 400 without a known reason is still something about the request.
		return toolerr.New(toolerr.CodeInvalidQuery, msg)
	}
	return toolerr.Newf(toolerr.CodeUpstreamError, "BigQuery answered HTTP %d: %s", status, msg)
}

// dailyQuota recognises the one quota that a retry cannot help: the
// per-day allowances, which BigQuery names in the message. This is the
// single text rule in the mapping, made on a fixed phrase (ADR-0004).
func dailyQuota(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(l, "per day") || strings.Contains(l, "daily")
}

func accessHint(msg string) string {
	return msg + " — the caller usually lacks roles/bigquery.jobUser on the billing project or roles/bigquery.dataViewer on the data; see get_usage"
}

func billingHint(msg string) string {
	return msg + " — the billing project has no billing account; the operator must enable billing or choose another billing project"
}

func apiHint(msg string) string {
	return msg + " — the BigQuery API is not enabled on the billing project; the operator enables it in the Cloud console"
}

func resourcesHint(msg string) string {
	return msg + " — rewrite the query to do less work (filter earlier, avoid ORDER BY over the full result, aggregate)"
}

func kernelCapHint(msg string) string {
	return msg + " — BigQuery stopped the job at maximumBytesBilled after the dry-run estimate passed; the estimate was a lower bound. Narrow the query."
}

func authHint(msg string) string {
	return msg + " — no usable Application Default Credentials; run `gcloud auth application-default login` and retry"
}

// authError wraps a token-source failure.
func authError(err error) *toolerr.Error {
	return toolerr.New(toolerr.CodeAuthError, authHint(err.Error()))
}

// transportError wraps a network-level failure (DNS, TLS, connection
// reset). It is retryable because every request this client sends is
// idempotent (client-named jobs, dry runs, reads).
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

// ProducedCodes lists every code mapReason can emit, for the test that
// keeps toolerr.Codes complete.
func ProducedCodes() []string {
	seen := map[string]bool{}
	var out []string
	add := func(c string) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, r := range reasonRules {
		add(r.code)
	}
	for _, c := range []string{toolerr.CodeTimeout, toolerr.CodeAuthError, toolerr.CodeAccessDenied, toolerr.CodeNotFound, toolerr.CodeDuplicate, toolerr.CodeRateLimited, toolerr.CodeBackendError, toolerr.CodeInvalidQuery, toolerr.CodeUpstreamError} {
		add(c)
	}
	return out
}
