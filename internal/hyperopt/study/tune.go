package study

// tune.go (spec §8.2): produce a tuned strategy param document by overlaying a
// best trial's params onto the PACKAGE baseline, replacing ONLY each tuned
// param's `default` value (search/constraints/ordering/all other fields
// preserved verbatim) and swapping the metadata block for the tuned provenance
// ({source:"tuned", created_at, tuned_from_study, tuned_from_trial}). The
// output is 2-space-indented JSON with insertion order preserved, emitted by
// the shared pyjson encoder.
//
// The promoted values are the OPTUNA-recorded (pre-constraint-clamp) values
// (§2.3/§8.1 step 4, Q5: bug-compatible — the runtime loader re-applies clamps).
// A tuned key absent from the baseline parameters is an error
// ("strategy '<s>' has no param '<n>'; cannot tune").

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
	"github.com/byjackchen/trade-tms-go/internal/runs"
)

// TuneInput parameterizes one strategy's best_params document.
type TuneInput struct {
	Strategy string
	// Tuned maps unprefixed param name -> promoted value (float64; ints are whole
	// floats, the Optuna-recorded shape).
	Tuned map[string]float64
	// StudyName / TrialNumber feed metadata.tuned_from_study / tuned_from_trial.
	StudyName   string
	TrialNumber int
	// Now is the metadata.created_at timestamp (UTC ISO).
	Now time.Time
}

// TuneBaseline reads the PACKAGE baseline for the strategy and returns the tuned
// document as 2-space-indented JSON bytes. The baseline order is preserved;
// only tuned defaults and the metadata block change.
func TuneBaseline(in TuneInput) ([]byte, error) {
	raw, err := hyperopt.BaselineRaw(in.Strategy)
	if err != nil {
		return nil, err
	}
	// Validate every tuned key exists in the baseline parameters (§8.2).
	sp, err := hyperopt.LoadBaselineParams(in.Strategy)
	if err != nil {
		return nil, err
	}
	for name := range in.Tuned {
		if _, ok := sp.Param(name); !ok {
			return nil, fmt.Errorf("strategy '%s' has no param '%s'; cannot tune", in.Strategy, name)
		}
	}

	// Decode the full baseline into an ordered tree, rewrite defaults + metadata,
	// re-encode with the pyjson encoder.
	var top orderedMap
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("hyperopt: parsing baseline %s: %w", in.Strategy, err)
	}

	// Rewrite parameters.<name>.default for each tuned key.
	paramsVal, ok := top.get("parameters")
	if !ok {
		return nil, fmt.Errorf("hyperopt: baseline %s missing parameters", in.Strategy)
	}
	paramsObj, ok := paramsVal.(*orderedMap)
	if !ok {
		return nil, fmt.Errorf("hyperopt: baseline %s parameters is not an object", in.Strategy)
	}
	for name, val := range in.Tuned {
		specVal, ok := paramsObj.get(name)
		if !ok {
			return nil, fmt.Errorf("strategy '%s' has no param '%s'; cannot tune", in.Strategy, name)
		}
		specObj := specVal.(*orderedMap)
		// Preserve int-ness: if the baseline declares this param "int", store an
		// integer default (no trailing ".0"); else a float.
		if ps, ok := sp.Param(name); ok && ps.Type == "int" {
			specObj.set("default", jsonInt(int64(val)))
		} else {
			specObj.set("default", jsonFloat(val))
		}
	}

	// Replace metadata with the tuned provenance, preserving any pre-existing
	// metadata keys (merge old <- new).
	meta, _ := top.get("metadata")
	newMeta := &orderedMap{}
	if mo, ok := meta.(*orderedMap); ok {
		for _, k := range mo.keys {
			v, _ := mo.get(k)
			newMeta.set(k, v)
		}
	}
	newMeta.set("source", jsonStr("tuned"))
	newMeta.set("created_at", jsonStr(isoUTC(in.Now)))
	newMeta.set("tuned_from_study", jsonStr(in.StudyName))
	newMeta.set("tuned_from_trial", jsonInt(int64(in.TrialNumber)))
	top.set("metadata", newMeta)

	return runs.Marshal(top.toPyValue()), nil
}

// ---------------------------------------------------------------------------
// orderedMap — an insertion-order-preserving JSON object decoder/encoder, so the
// tuned document keeps the baseline's exact key order (sort_keys=False).
// ---------------------------------------------------------------------------

type orderedMap struct {
	keys []string
	vals map[string]any
}

func (o *orderedMap) get(k string) (any, bool) {
	if o.vals == nil {
		return nil, false
	}
	v, ok := o.vals[k]
	return v, ok
}

func (o *orderedMap) set(k string, v any) {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, exists := o.vals[k]; !exists {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// jsonStr/jsonInt/jsonFloat are typed scalar markers so toPyValue renders them
// with the correct surface form (ints without ".0", floats with shortest repr).
type jsonStr string
type jsonInt int64
type jsonFloat float64

// UnmarshalJSON decodes a JSON object preserving key order via a token stream.
// UseNumber keeps number literals as json.Number so int defaults (e.g.
// history_max_bars: 1000) round-trip without a spurious ".0".
func (o *orderedMap) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("expected object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key := keyTok.(string)
		v, err := decodeOrderedValue(dec)
		if err != nil {
			return err
		}
		o.set(key, v)
	}
	_, err = dec.Token() // closing '}'
	return err
}

// decodeOrderedValue decodes the next JSON value, recursing into objects as
// orderedMaps so nested order is preserved.
func decodeOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			om := &orderedMap{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key := keyTok.(string)
				v, err := decodeOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				om.set(key, v)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return om, nil
		case '[':
			var arr []any
			for dec.More() {
				v, err := decodeOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return arr, nil
		}
	case string:
		return jsonStr(t), nil
	case bool:
		return t, nil
	case float64:
		// Only reached if UseNumber is off (it isn't); kept for safety.
		return jsonFloat(t), nil
	case json.Number:
		return numberToScalar(t), nil
	case nil:
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// toPyValue converts the ordered tree into a runs.PyValue for encoding.
func (o *orderedMap) toPyValue() runs.PyValue {
	obj := runs.NewObj()
	for _, k := range o.keys {
		obj.Set(k, scalarToPy(o.vals[k]))
	}
	return obj
}

func scalarToPy(v any) runs.PyValue {
	switch x := v.(type) {
	case *orderedMap:
		return x.toPyValue()
	case []any:
		a := runs.NewArr()
		for _, it := range x {
			a.Append(scalarToPy(it))
		}
		return a
	case jsonStr:
		return runs.Str(string(x))
	case jsonInt:
		return runs.Int(int64(x))
	case jsonFloat:
		return runs.PyFloat(float64(x))
	case bool:
		return runs.Bool(x)
	case nil:
		return runs.Null{}
	default:
		return runs.Str(fmt.Sprintf("%v", x))
	}
}

// numberToScalar classifies a json.Number literal as int (no '.'/'e') or float.
func numberToScalar(n json.Number) any {
	s := string(n)
	isFloat := false
	for _, c := range s {
		if c == '.' || c == 'e' || c == 'E' {
			isFloat = true
			break
		}
	}
	if isFloat {
		f, _ := n.Float64()
		return jsonFloat(f)
	}
	i, err := n.Int64()
	if err != nil {
		f, _ := n.Float64()
		return jsonFloat(f)
	}
	return jsonInt(i)
}
