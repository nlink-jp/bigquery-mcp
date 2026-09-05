package bq

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Row is one decoded result row: column name → value.
type Row map[string]any

// decodeRows turns wire rows (f[].v in schema order) into column-keyed
// objects following the schema recursively (architecture.md, "Row
// decoding"). Timestamps are expected as int64 microseconds because every
// request sets formatOptions.useInt64Timestamp.
func decodeRows(schema *TableSchema, rows []tableRow) ([]Row, error) {
	if schema == nil {
		return nil, fmt.Errorf("result has rows but no schema")
	}
	out := make([]Row, 0, len(rows))
	for i, r := range rows {
		obj, err := decodeRecord(schema.Fields, r.F)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		out = append(out, obj)
	}
	return out, nil
}

func decodeRecord(fields []*FieldSchema, cells []tableCell) (Row, error) {
	if len(cells) != len(fields) {
		return nil, fmt.Errorf("%d cells for %d fields", len(cells), len(fields))
	}
	obj := make(Row, len(fields))
	for i, f := range fields {
		v, err := decodeCell(f, cells[i].V)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Name, err)
		}
		obj[f.Name] = v
	}
	return obj, nil
}

// decodeCell decodes one cell value according to its field schema.
func decodeCell(f *FieldSchema, raw json.RawMessage) (any, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	if f.Mode == "REPEATED" {
		var items []tableCell
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("repeated value: %w", err)
		}
		arr := make([]any, 0, len(items))
		for _, it := range items {
			v, err := decodeScalarOrRecord(f, it.V)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	}
	return decodeScalarOrRecord(f, raw)
}

func decodeScalarOrRecord(f *FieldSchema, raw json.RawMessage) (any, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	switch f.Type {
	case "RECORD", "STRUCT":
		var rec tableRow
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("record value: %w", err)
		}
		return decodeRecord(f.Fields, rec.F)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// Not a string: BigQuery cells are strings for every scalar type;
		// anything else is passed through as-is rather than lost.
		var v any
		if err2 := json.Unmarshal(raw, &v); err2 != nil {
			return nil, fmt.Errorf("cell value: %w", err)
		}
		return v, nil
	}
	return decodeScalar(f.Type, s), nil
}

// decodeScalar maps a string cell to a JSON-friendly Go value.
func decodeScalar(typ, s string) any {
	switch typ {
	case "INTEGER", "INT64":
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			if n >= -(1<<53) && n <= 1<<53 {
				return n
			}
		}
		return s // beyond 53 bits: keep exact as a string
	case "FLOAT", "FLOAT64":
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
		return s // NaN, Infinity: JSON cannot carry them as numbers
	case "BOOLEAN", "BOOL":
		return s == "true"
	case "TIMESTAMP":
		if us, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.UnixMicro(us).UTC().Format(time.RFC3339Nano)
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil { // legacy float seconds
			sec, frac := math.Modf(f)
			return time.Unix(int64(sec), int64(frac*1e9)).UTC().Format(time.RFC3339Nano)
		}
		return s
	case "JSON":
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		return s
	}
	// STRING, BYTES (base64), DATE, TIME, DATETIME, NUMERIC, BIGNUMERIC,
	// GEOGRAPHY, INTERVAL, RANGE: the string BigQuery gave.
	return s
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// parseInt64 reads a numeric string field ("123") and tolerates absence.
func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
