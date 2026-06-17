package sharadar

// wire.go decodes the Nasdaq Data Link "datatables" JSON wire format — the
// payload the get_table endpoint returns
// (spec docs/spec/data-sharadar.md §3.1):
//
//	{
//	  "datatable": {
//	    "data":    [["AAPL", "2024-01-02", 185.0, ...], ...],
//	    "columns": [{"name": "ticker", "type": "String"},
//	                {"name": "date",   "type": "Date"}, ...]
//	  },
//	  "meta": {"next_cursor_id": "djF8..." | null}
//	}
//
// Decoding is streaming at the row level: a json.Decoder walks tokens and
// decodes one data row at a time, so memory is bounded by a single page
// (the server caps pages at qopts.per_page rows; the client requests
// 10,000), never by the whole logical result. Because the live API emits
// "data" before "columns", rows of the current page are buffered until the
// column list arrives; both key orders are accepted.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// wireColumn is one entry of datatable.columns.
type wireColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// page is one decoded cursor page.
type page struct {
	columns    []wireColumn
	rows       [][]any
	nextCursor string
}

// colIndex builds the lower-cased name -> position map for Row access.
func (p *page) colIndex() map[string]int {
	m := make(map[string]int, len(p.columns))
	for i, c := range p.columns {
		m[strings.ToLower(c.Name)] = i
	}
	return m
}

// decodePage stream-decodes one datatables JSON page.
func decodePage(r io.Reader) (*page, error) {
	dec := json.NewDecoder(r)
	p := &page{}

	if err := expectDelim(dec, '{'); err != nil {
		return nil, fmt.Errorf("sharadar: decoding response: %w", err)
	}
	for dec.More() {
		key, err := stringToken(dec)
		if err != nil {
			return nil, fmt.Errorf("sharadar: decoding response: %w", err)
		}
		switch key {
		case "datatable":
			if err := decodeDatatable(dec, p); err != nil {
				return nil, err
			}
		case "meta":
			var meta struct {
				NextCursorID *string `json:"next_cursor_id"`
			}
			if err := dec.Decode(&meta); err != nil {
				return nil, fmt.Errorf("sharadar: decoding meta: %w", err)
			}
			if meta.NextCursorID != nil {
				p.nextCursor = *meta.NextCursorID
			}
		default:
			if err := skipValue(dec); err != nil {
				return nil, fmt.Errorf("sharadar: decoding response: %w", err)
			}
		}
	}
	// Closing '}' is consumed implicitly when More() returns false; verify.
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("sharadar: decoding response close: %w", err)
	}
	if p.columns == nil {
		return nil, fmt.Errorf("sharadar: response has no datatable.columns")
	}
	return p, nil
}

func decodeDatatable(dec *json.Decoder, p *page) error {
	if err := expectDelim(dec, '{'); err != nil {
		return fmt.Errorf("sharadar: decoding datatable: %w", err)
	}
	for dec.More() {
		key, err := stringToken(dec)
		if err != nil {
			return fmt.Errorf("sharadar: decoding datatable: %w", err)
		}
		switch key {
		case "data":
			if err := expectDelim(dec, '['); err != nil {
				return fmt.Errorf("sharadar: decoding datatable.data: %w", err)
			}
			for dec.More() {
				var row []any
				if err := dec.Decode(&row); err != nil {
					return fmt.Errorf("sharadar: decoding data row %d: %w", len(p.rows), err)
				}
				p.rows = append(p.rows, row)
			}
			if err := expectDelim(dec, ']'); err != nil {
				return fmt.Errorf("sharadar: decoding datatable.data close: %w", err)
			}
		case "columns":
			if err := dec.Decode(&p.columns); err != nil {
				return fmt.Errorf("sharadar: decoding datatable.columns: %w", err)
			}
		default:
			if err := skipValue(dec); err != nil {
				return fmt.Errorf("sharadar: decoding datatable: %w", err)
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("sharadar: decoding datatable close: %w", err)
	}
	return nil
}

func expectDelim(dec *json.Decoder, d json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	got, ok := tok.(json.Delim)
	if !ok || got != d {
		return fmt.Errorf("expected %q, got %v", d, tok)
	}
	return nil
}

func stringToken(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	s, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("expected object key, got %v", tok)
	}
	return s, nil
}

// skipValue discards the next complete JSON value.
func skipValue(dec *json.Decoder) error {
	var raw json.RawMessage
	return dec.Decode(&raw)
}

// ---------------------------------------------------------------------------
// Row — one API data row with by-name column access
// ---------------------------------------------------------------------------

// Row is one Sharadar API row. Values carry encoding/json's natural types
// (string, float64, bool, nil); accessors do the tolerant coercions the
// downstream converters require.
type Row struct {
	cols map[string]int // lower-cased column name -> position
	vals []any
}

// Has reports whether the column exists in this result set (regardless of
// the cell being null).
func (r Row) Has(name string) bool {
	i, ok := r.cols[name]
	return ok && i < len(r.vals)
}

func (r Row) cell(name string) (any, bool) {
	i, ok := r.cols[name]
	if !ok || i >= len(r.vals) {
		return nil, false
	}
	v := r.vals[i]
	if v == nil {
		return nil, false
	}
	return v, true
}

// Str returns the cell as a string. Numeric cells are formatted (integral
// floats without exponent) so e.g. an eventcodes value of 22 round-trips as
// "22"; null/missing yields ("", false).
func (r Row) Str(name string) (string, bool) {
	v, ok := r.cell(name)
	if !ok {
		return "", false
	}
	switch s := v.(type) {
	case string:
		return s, true
	case float64:
		if s == math.Trunc(s) && !math.IsInf(s, 0) && math.Abs(s) < 1e15 {
			return strconv.FormatInt(int64(s), 10), true
		}
		return strconv.FormatFloat(s, 'f', -1, 64), true
	case bool:
		if s {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// Float returns the cell as float64; string-encoded numbers are parsed
// (pandas' read coercion); null/missing/unparseable yields (0, false).
func (r Row) Float(name string) (float64, bool) {
	v, ok := r.cell(name)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// dateLayouts are the temporal encodings observed on the datatables wire
// (Date columns as "2006-01-02", DateTime as "2006-01-02T15:04:05[.frac]").
var dateLayouts = []string{
	"2006-01-02",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
}

// Date returns the cell as a tz-naive-equivalent UTC instant. Empty strings
// and unparseable values coerce to (zero, false) — the pandas
// to_datetime(errors="coerce") behavior the reader relies on (spec §7.2).
func (r Row) Date(name string) (time.Time, bool) {
	s, ok := r.Str(name)
	if !ok || strings.TrimSpace(s) == "" {
		return time.Time{}, false
	}
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
