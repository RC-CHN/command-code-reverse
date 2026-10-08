package stream

import (
	"encoding/json"
	"math"
	"strconv"
)

// TokenCount is an optional nonnegative integer counter. Invalid optional
// accounting must not discard an otherwise valid stream event. Valid
// distinguishes an explicit zero from an absent or malformed value.
type TokenCount struct {
	Value int
	Valid bool
}

// UnmarshalJSON also accepts integral decimal/exponent notation. Fractional
// or out-of-range values cannot be represented by the proxy's token counters
// and are ignored rather than rounded, wrapped or treated as stream failures.
func (c *TokenCount) UnmarshalJSON(data []byte) error {
	*c = TokenCount{}
	var n *float64
	if err := json.Unmarshal(data, &n); err != nil || n == nil {
		return nil
	}
	if *n < 0 || math.Trunc(*n) != *n || *n >= math.Ldexp(1, strconv.IntSize-1) {
		return nil
	}
	*c = TokenCount{Value: int(*n), Valid: true}
	return nil
}
