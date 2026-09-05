package toolerr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

func TestErrorString(t *testing.T) {
	e := toolerr.New(toolerr.CodeBudgetExceeded, "12 GiB above the 10 GiB budget")
	if got := e.Error(); got != "budget_exceeded: 12 GiB above the 10 GiB budget" {
		t.Errorf("got %q", got)
	}
	if got := toolerr.New(toolerr.CodeTimeout, "").Error(); got != "timeout" {
		t.Errorf("empty message should print the code alone, got %q", got)
	}
}

func TestErrorIsByCode(t *testing.T) {
	sentinel := toolerr.New(toolerr.CodeInvalidQuery, "")
	actual := toolerr.Newf(toolerr.CodeInvalidQuery, "Unrecognized name: %s", "emial")
	if !errors.Is(actual, sentinel) {
		t.Errorf("errors.Is should match by Code")
	}
	other := toolerr.New(toolerr.CodeNotFound, "")
	if errors.Is(actual, other) {
		t.Errorf("errors.Is should not match a different Code")
	}
}

func TestErrorWrappedIs(t *testing.T) {
	inner := toolerr.New(toolerr.CodeBackendError, "backendError")
	wrapped := fmt.Errorf("jobs.query: %w", inner)
	if !errors.Is(wrapped, toolerr.New(toolerr.CodeBackendError, "")) {
		t.Errorf("errors.Is should walk the wrapper chain")
	}
}

func TestErrorJSONMarshal(t *testing.T) {
	e := toolerr.New(toolerr.CodeAccessDenied, "boom").WithDetails(map[string]any{
		"reason": "accessDenied",
		"table":  "p.d.t",
	})
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"code":"access_denied"`, `"message":"boom"`, `"retryable":false`, `"reason":"accessDenied"`} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled error missing %q: %s", want, s)
		}
	}
}

func TestRetryableIsAlwaysPresentAndCopied(t *testing.T) {
	base := toolerr.New(toolerr.CodeRateLimited, "slow down")
	r := base.AsRetryable()
	if base.Retryable {
		t.Errorf("AsRetryable must not mutate the receiver")
	}
	if !r.Retryable {
		t.Errorf("AsRetryable should set the flag on the copy")
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"retryable":true`) {
		t.Errorf("retryable flag missing from JSON: %s", b)
	}
	if !errors.Is(r, base) {
		t.Errorf("retryable copy must still match its code")
	}
}

func TestWithDetailsDoesNotMutate(t *testing.T) {
	e := toolerr.New(toolerr.CodeInvalidArguments, "x")
	_ = e.WithDetails(map[string]any{"k": "v"})
	if e.Details != nil {
		t.Errorf("WithDetails should not mutate receiver")
	}
}
