package runs

// pyjson.go renders JSON byte-compatible with the Python reference's
// json.dumps(payload, indent=2, default=str) used by src/runs/dumper.py
// (api-ws-redis.md §7 [MUST-MATCH]). Two things differ from Go's
// encoding/json and must be reproduced:
//
//   - Float formatting follows Python's repr (shortest round-trip), but with
//     Python's distinctive surface form: a whole-number float keeps a trailing
//     ".0" (100000.0, not 100000); exponents use Python's e+NN / e-NN form with
//     a sign and at least two exponent digits. This is the same shortest-digits
//     algorithm Go's strconv uses, re-surfaced into Python's layout. (Q2 of the
//     api-ws-redis spec asks for byte-equality of these floats; we provide it.)
//   - HTML escaping is off (Python does not escape <, >, &).
//
// Everything else (2-space indent, key insertion order via PyValue ordering,
// no trailing newline) is handled by the encoder below. Strings, bools, ints
// and nested maps/arrays use standard JSON.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// PyFloat is a float64 that JSON-encodes with Python repr surface form. Use it
// for every float value that must be byte-compatible with a Python json.dumps
// dump (balances, prices, pnl).
type PyFloat float64

// PyValue is the artifact value model: it preserves insertion order for object
// keys (Python dicts serialize in insertion order). Build artifacts out of
// Obj/Arr/these scalar wrappers and encode with Marshal.
type PyValue interface {
	writeTo(b *strings.Builder, indent int)
}

// Obj is an ordered JSON object.
type Obj struct {
	keys []string
	vals []PyValue
}

// NewObj returns an empty ordered object.
func NewObj() *Obj { return &Obj{} }

// Set appends (or overwrites) key with v, preserving first-insertion order.
func (o *Obj) Set(key string, v PyValue) *Obj {
	for i, k := range o.keys {
		if k == key {
			o.vals[i] = v
			return o
		}
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, v)
	return o
}

// Len reports the number of keys.
func (o *Obj) Len() int { return len(o.keys) }

// Arr is a JSON array.
type Arr struct{ items []PyValue }

// NewArr returns an empty array.
func NewArr() *Arr { return &Arr{} }

// Append adds v to the array.
func (a *Arr) Append(v PyValue) *Arr { a.items = append(a.items, v); return a }

// Len reports the array length.
func (a *Arr) Len() int { return len(a.items) }

// Scalar wrappers.

// Str is a JSON string.
type Str string

// Int is a JSON integer.
type Int int64

// Bool is a JSON boolean.
type Bool bool

// Null is JSON null.
type Null struct{}

// Raw is a pre-serialized JSON fragment (already compact/valid); used to embed
// opaque blobs (e.g. a JSONB params object) verbatim.
type Raw string

// Marshal renders v as Python-json.dumps(indent=2)-compatible bytes (no
// trailing newline).
func Marshal(v PyValue) []byte {
	var b strings.Builder
	v.writeTo(&b, 0)
	return []byte(b.String())
}

func writeIndent(b *strings.Builder, indent int) {
	for i := 0; i < indent; i++ {
		b.WriteString("  ")
	}
}

func (o *Obj) writeTo(b *strings.Builder, indent int) {
	if len(o.keys) == 0 {
		b.WriteString("{}")
		return
	}
	b.WriteString("{\n")
	for i, k := range o.keys {
		writeIndent(b, indent+1)
		b.WriteString(encodePyString(k))
		b.WriteString(": ")
		o.vals[i].writeTo(b, indent+1)
		if i < len(o.keys)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	writeIndent(b, indent)
	b.WriteByte('}')
}

func (a *Arr) writeTo(b *strings.Builder, indent int) {
	if len(a.items) == 0 {
		b.WriteString("[]")
		return
	}
	b.WriteString("[\n")
	for i, it := range a.items {
		writeIndent(b, indent+1)
		it.writeTo(b, indent+1)
		if i < len(a.items)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	writeIndent(b, indent)
	b.WriteByte(']')
}

func (s Str) writeTo(b *strings.Builder, _ int) { b.WriteString(encodePyString(string(s))) }
func (n Int) writeTo(b *strings.Builder, _ int) { b.WriteString(strconv.FormatInt(int64(n), 10)) }
func (Null) writeTo(b *strings.Builder, _ int)  { b.WriteString("null") }
func (r Raw) writeTo(b *strings.Builder, _ int) { b.WriteString(string(r)) }
func (bl Bool) writeTo(b *strings.Builder, _ int) {
	if bool(bl) {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
}

func (f PyFloat) writeTo(b *strings.Builder, _ int) { b.WriteString(FormatPyFloat(float64(f))) }

// FormatPyFloat renders x the way Python's json.dumps / repr does:
//   - shortest round-trip digits (same algorithm Go's strconv 'g'/-1 uses);
//   - whole numbers keep a trailing ".0" (100000.0);
//   - exponential form uses "e+NN"/"e-NN" with a sign and >= 2 exponent digits;
//   - NaN/Inf become Python's "NaN"/"Infinity"/"-Infinity" (json.dumps default
//     allow_nan=True; the reference never emits these but we stay faithful).
func FormatPyFloat(x float64) string {
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "Infinity"
	case math.IsInf(x, -1):
		return "-Infinity"
	}
	// Python uses repr(float) == float_repr_style 'short': shortest string that
	// round-trips, choosing fixed vs scientific by the same thresholds as
	// strconv 'g' with -1 precision EXCEPT Python switches to exponent for
	// exp < -4 or exp >= 16. Go's 'g' switches at exp < -4 or exp >= 21.
	// Reconcile by formatting with 'g' then post-processing to Python's layout.
	s := strconv.FormatFloat(x, 'g', -1, 64)
	return pyFloatSurface(s, x)
}

// pyFloatSurface rewrites Go's 'g' output into Python's repr surface form.
func pyFloatSurface(s string, x float64) string {
	// Determine the decimal exponent of x to apply Python's fixed/scientific
	// threshold (>= 1e16 or < 1e-4 -> scientific).
	if x == 0 {
		if math.Signbit(x) {
			return "-0.0"
		}
		return "0.0"
	}
	mant, exp := shortestDigits(x)
	// mant is the shortest significant-digit string (no sign, no dot); exp is
	// the power of ten of the first digit. Python repr: use scientific when
	// exp < -4 or exp >= 16; otherwise fixed.
	neg := math.Signbit(x)
	var out string
	if exp < -4 || exp >= 16 {
		out = pySci(mant, exp)
	} else {
		out = pyFixed(mant, exp)
	}
	if neg {
		out = "-" + out
	}
	// Defensive: if our reconstruction does not round-trip, fall back to a
	// massaged 'g' form (guarantees validity; loses exact Python layout only in
	// pathological cases none of the reference values hit).
	if v, err := strconv.ParseFloat(out, 64); err != nil || v != x {
		return ensureDotOrExp(s)
	}
	return out
}

// shortestDigits returns the shortest round-tripping significant digits of
// |x| and the base-10 exponent of the leading digit. E.g. 100000.0 -> ("1", 5),
// 5247.309999999998 -> ("5247309999999998", 3), 2.8e12 -> ("28", 12).
func shortestDigits(x float64) (mant string, exp int) {
	e := strconv.FormatFloat(math.Abs(x), 'e', -1, 64) // d.ddde±NN
	mantPart, expPart, _ := strings.Cut(e, "e")
	exp, _ = strconv.Atoi(expPart)
	mantPart = strings.Replace(mantPart, ".", "", 1)
	mantPart = strings.TrimRight(mantPart, "0")
	if mantPart == "" {
		mantPart = "0"
	}
	return mantPart, exp
}

// pyFixed renders mant*10^(exp) in Python fixed-point repr, always keeping a
// decimal point (a trailing ".0" for whole numbers).
func pyFixed(mant string, exp int) string {
	digits := mant
	if exp >= 0 {
		if exp+1 >= len(digits) {
			// Integer value: pad zeros, append ".0".
			return digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
		}
		return digits[:exp+1] + "." + digits[exp+1:]
	}
	// exp < 0: leading "0." then |exp|-1 zeros then digits.
	return "0." + strings.Repeat("0", -exp-1) + digits
}

// pySci renders mant*10^exp in Python scientific repr: "de±NN" or "d.dddde±NN"
// with a signed exponent of at least two digits. A single-digit mantissa
// carries NO decimal point (Python: 1e+16, not 1.0e+16).
func pySci(mant string, exp int) string {
	var m string
	if len(mant) == 1 {
		m = mant
	} else {
		m = mant[:1] + "." + mant[1:]
	}
	sign := "+"
	e := exp
	if e < 0 {
		sign = "-"
		e = -e
	}
	es := strconv.Itoa(e)
	if len(es) < 2 {
		es = "0" + es
	}
	return m + "e" + sign + es
}

// ensureDotOrExp guarantees a 'g'-formatted string looks like a float (has a
// '.' or 'e'); used only on the defensive fallback path.
func ensureDotOrExp(s string) string {
	if strings.ContainsAny(s, ".eE") {
		return s
	}
	return s + ".0"
}

// encodePyString encodes a Go string the way Python json.dumps does for ASCII
// (ensure_ascii=True is Python's default, but the reference data is all ASCII
// and the spec notes "non-ASCII unescaped is irrelevant"). We escape the JSON
// mandatory set and keep printable ASCII verbatim. Non-ASCII is emitted as
// \uXXXX to match Python's default ensure_ascii.
func encodePyString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r > 0x7e {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// SortedKeys returns the keys of m sorted ascending (helper for deterministic
// object construction from Go maps).
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
