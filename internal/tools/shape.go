package tools

import (
	"encoding/json"

	"github.com/nlink-jp/bigquery-mcp/internal/bq"
)

// envelopeReserve is the response bytes kept for everything that is not a
// row (schema, counters, warnings) when applying max_bytes.
const envelopeReserve = 4 << 10

// shaper is the sink of ADR-0003: it keeps rows while both caps hold and
// records which cap ended the result.
type shaper struct {
	maxRows  int
	maxBytes int64
	rows     []json.RawMessage
	size     int64
	// truncatedBy is "" while no cap was hit, else "max_rows" or "max_bytes".
	truncatedBy string
}

func newShaper(maxRows int, maxBytes int64) *shaper {
	if maxBytes > envelopeReserve {
		maxBytes -= envelopeReserve
	}
	return &shaper{maxRows: maxRows, maxBytes: maxBytes}
}

// sink is handed to bq.Query. It returns false when the next row would
// break a cap, which stops paging.
func (s *shaper) sink(r bq.Row) bool {
	if len(s.rows) >= s.maxRows {
		s.truncatedBy = "max_rows"
		return false
	}
	b, err := json.Marshal(r)
	if err != nil {
		// A row that will not serialise is reported as such rather than
		// dropped silently.
		b = []byte(`{"_error":"row could not be serialised"}`)
	}
	next := s.size + int64(len(b)) + 1
	if len(s.rows) > 0 && next > s.maxBytes {
		s.truncatedBy = "max_bytes"
		return false
	}
	s.rows = append(s.rows, b)
	s.size = next
	if len(s.rows) >= s.maxRows {
		// The cap is reached exactly; whether that truncates depends on
		// total_rows, decided by the caller.
		return false
	}
	return true
}
