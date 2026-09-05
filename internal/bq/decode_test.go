package bq

import (
	"encoding/json"
	"reflect"
	"testing"
)

func schemaOf(js string) *TableSchema {
	var s TableSchema
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		panic(err)
	}
	return &s
}

func rowsOf(js string) []tableRow {
	var r []tableRow
	if err := json.Unmarshal([]byte(js), &r); err != nil {
		panic(err)
	}
	return r
}

func TestDecodeScalarMatrix(t *testing.T) {
	schema := schemaOf(`{"fields":[
		{"name":"s","type":"STRING"},{"name":"i","type":"INTEGER"},{"name":"big","type":"INT64"},
		{"name":"f","type":"FLOAT"},{"name":"nan","type":"FLOAT64"},{"name":"b","type":"BOOLEAN"},
		{"name":"ts","type":"TIMESTAMP"},{"name":"d","type":"DATE"},{"name":"num","type":"NUMERIC"},
		{"name":"by","type":"BYTES"},{"name":"j","type":"JSON"},{"name":"geo","type":"GEOGRAPHY"},
		{"name":"n","type":"STRING"}]}`)
	rows := rowsOf(`[{"f":[{"v":"hi"},{"v":"42"},{"v":"9007199254740993"},{"v":"1.5"},{"v":"NaN"},{"v":"true"},
		{"v":"1788617399000000"},{"v":"2026-09-05"},{"v":"12.34"},{"v":"aGk="},{"v":"{\"a\":1}"},{"v":"POINT(1 2)"},{"v":null}]}]`)
	out, err := decodeRows(schema, rows)
	if err != nil {
		t.Fatal(err)
	}
	r := out[0]
	want := Row{
		"s": "hi", "i": int64(42), "big": "9007199254740993", "f": 1.5, "nan": "NaN", "b": true,
		"ts": "2026-09-05T14:09:59Z", "d": "2026-09-05", "num": "12.34", "by": "aGk=",
		"j": json.RawMessage(`{"a":1}`), "geo": "POINT(1 2)", "n": nil,
	}
	for k, v := range want {
		if !reflect.DeepEqual(r[k], v) {
			t.Errorf("%s = %#v (%T), want %#v", k, r[k], r[k], v)
		}
	}
	if len(r) != len(want) {
		t.Errorf("row has %d keys, want %d", len(r), len(want))
	}
}

func TestDecodeLegacyFloatTimestamp(t *testing.T) {
	schema := schemaOf(`{"fields":[{"name":"ts","type":"TIMESTAMP"}]}`)
	out, err := decodeRows(schema, rowsOf(`[{"f":[{"v":"1.7886173995E9"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if out[0]["ts"] != "2026-09-05T14:09:59.5Z" {
		t.Errorf("got %v", out[0]["ts"])
	}
}

func TestDecodeNestedAndRepeated(t *testing.T) {
	schema := schemaOf(`{"fields":[
		{"name":"email","type":"STRING"},
		{"name":"token","type":"RECORD","fields":[{"name":"app","type":"STRING"},{"name":"scopes","type":"STRING","mode":"REPEATED"}]},
		{"name":"events","type":"RECORD","mode":"REPEATED","fields":[{"name":"name","type":"STRING"},{"name":"n","type":"INTEGER"}]},
		{"name":"tags","type":"STRING","mode":"REPEATED"},
		{"name":"empty","type":"STRING","mode":"REPEATED"},
		{"name":"nullrec","type":"RECORD","fields":[{"name":"x","type":"STRING"}]}]}`)
	rows := rowsOf(`[{"f":[
		{"v":"a@x"},
		{"v":{"f":[{"v":"cal"},{"v":[{"v":"r"},{"v":"w"}]}]}},
		{"v":[{"v":{"f":[{"v":"login"},{"v":"2"}]}},{"v":{"f":[{"v":"logout"},{"v":null}]}}]},
		{"v":[{"v":"t1"}]},
		{"v":[]},
		{"v":null}]}]`)
	out, err := decodeRows(schema, rows)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out[0])
	want := `{"email":"a@x","empty":[],"events":[{"n":2,"name":"login"},{"n":null,"name":"logout"}],"nullrec":null,"tags":["t1"],"token":{"app":"cal","scopes":["r","w"]}}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestDecodeRejectsShapeMismatch(t *testing.T) {
	schema := schemaOf(`{"fields":[{"name":"a","type":"STRING"},{"name":"b","type":"STRING"}]}`)
	if _, err := decodeRows(schema, rowsOf(`[{"f":[{"v":"only one"}]}]`)); err == nil {
		t.Errorf("cell/field count mismatch must be an error, not silent loss")
	}
	if _, err := decodeRows(nil, rowsOf(`[{"f":[{"v":"x"}]}]`)); err == nil {
		t.Errorf("rows without a schema must be an error")
	}
}
