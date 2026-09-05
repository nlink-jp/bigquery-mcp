package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
	"github.com/nlink-jp/bigquery-mcp/internal/toolerr"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// scalarParamTypes are the BigQuery scalar parameter types accepted in
// the explicit {type, value} form.
var scalarParamTypes = map[string]bool{
	"STRING": true, "BYTES": true, "INT64": true, "FLOAT64": true, "NUMERIC": true, "BIGNUMERIC": true,
	"BOOL": true, "DATE": true, "TIME": true, "DATETIME": true, "TIMESTAMP": true, "GEOGRAPHY": true, "JSON": true,
}

// buildParams turns the `params` argument — an object of name → value —
// into named query parameters. A value is either a JSON scalar, typed by
// inference (bool → BOOL, integral number → INT64, other number →
// FLOAT64, string → STRING), or an explicit {"type": "DATE", "value":
// "2026-09-05"} object for the other scalar types (review finding 9).
func buildParams(raw map[string]json.RawMessage) ([]bq.QueryParameter, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]bq.QueryParameter, 0, len(names))
	for _, n := range names {
		typ, val, err := paramOf(n, raw[n])
		if err != nil {
			return nil, err
		}
		out = append(out, bq.QueryParameter{
			Name:           n,
			ParameterType:  bq.QueryParameterType{Type: typ},
			ParameterValue: bq.QueryParameterValue{Value: val},
		})
	}
	return out, nil
}

func paramOf(name string, v json.RawMessage) (typ, val string, err error) {
	var s string
	var b bool
	var f float64
	var explicit struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	switch {
	case json.Unmarshal(v, &s) == nil:
		return "STRING", s, nil
	case json.Unmarshal(v, &b) == nil:
		return "BOOL", strconv.FormatBool(b), nil
	case json.Unmarshal(v, &f) == nil:
		if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return "INT64", strconv.FormatInt(int64(f), 10), nil
		}
		return "FLOAT64", strconv.FormatFloat(f, 'g', -1, 64), nil
	case json.Unmarshal(v, &explicit) == nil && explicit.Type != "":
		t := strings.ToUpper(explicit.Type)
		if !scalarParamTypes[t] {
			return "", "", toolerr.Newf(toolerr.CodeInvalidArguments,
				"params.%s: type %q is not a supported scalar type (%s)", name, explicit.Type, strings.Join(sortedKeys(scalarParamTypes), ", "))
		}
		var vs string
		if json.Unmarshal(explicit.Value, &vs) == nil {
			return t, vs, nil
		}
		var vf float64
		if json.Unmarshal(explicit.Value, &vf) == nil {
			return t, strconv.FormatFloat(vf, 'g', -1, 64), nil
		}
		var vb bool
		if json.Unmarshal(explicit.Value, &vb) == nil {
			return t, strconv.FormatBool(vb), nil
		}
		return "", "", toolerr.Newf(toolerr.CodeInvalidArguments, "params.%s: value must be a string, number or boolean", name)
	}
	return "", "", toolerr.Newf(toolerr.CodeInvalidArguments,
		"params.%s must be a string, number, boolean, or {\"type\": ..., \"value\": ...} (arrays and structs are not supported in v1)", name)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// schemaOut flattens a TableSchema for the response.
type fieldOut struct {
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Mode        string     `json:"mode,omitempty"`
	Description string     `json:"description,omitempty"`
	Fields      []fieldOut `json:"fields,omitempty"`
}

func schemaOut(s *bq.TableSchema) []fieldOut {
	if s == nil {
		return nil
	}
	return fieldsOut(s.Fields)
}

func fieldsOut(fs []*bq.FieldSchema) []fieldOut {
	out := make([]fieldOut, 0, len(fs))
	for _, f := range fs {
		if f == nil {
			continue
		}
		o := fieldOut{Name: f.Name, Type: f.Type, Mode: f.Mode, Description: f.Description}
		if len(f.Fields) > 0 {
			o.Fields = fieldsOut(f.Fields)
		}
		out = append(out, o)
	}
	return out
}

func tableNames(refs []bq.TableRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.String())
	}
	return out
}

func routineNames(refs []bq.RoutineRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.String())
	}
	return out
}

func paramsOut(ps []bq.QueryParameter) []map[string]string {
	out := make([]map[string]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, map[string]string{"name": p.Name, "type": p.ParameterType.Type})
	}
	return out
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// msToRFC3339 renders a millisecond epoch string ("1788617399000") as RFC 3339.
func msToRFC3339(s string) string {
	if s == "" {
		return ""
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return s
	}
	return unixMilliRFC3339(n)
}

func daysFromMs(s string) float64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return math.Round(float64(n)/86400000*100) / 100
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
