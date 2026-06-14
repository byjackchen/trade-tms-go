package livetrade

import (
	"encoding/json"
	"strconv"
)

// marshalState serialises a strategy's StateDictJSON() value to JSON bytes for
// the StateStore. A nil value marshals to "null"; the store skips empty states.
func marshalState(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// pctString renders a fraction (e.g. 0.10) as a human percent string ("10%")
// for halt reason text. Trailing zeros are trimmed.
func pctString(frac float64) string {
	s := strconv.FormatFloat(frac*100, 'f', -1, 64)
	return s + "%"
}
