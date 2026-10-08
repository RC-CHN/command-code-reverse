package stream

import (
	"fmt"
	"strings"
	"testing"
)

func TestCacheWriteTokenDecoding(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        int
		valid       bool
	}{
		{"positive", "37", 37, true},
		{"zero", "0", 0, true},
		{"decimal integer", "37.0", 37, true},
		{"exponent", "3.7e1", 37, true},
		{"null", "null", 0, false},
		{"negative", "-1", 0, false},
		{"fraction", "1.5", 0, false},
		{"string", `"37"`, 0, false},
		{"boolean", "true", 0, false},
		{"object", "{}", 0, false},
		{"array", "[]", 0, false},
		{"float overflow", "1e400", 0, false},
		{"integer overflow", "9223372036854775808", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, nested := range []bool{false, true} {
				line := fmt.Sprintf(`{"type":"cache-write-tokens","cacheWriteTokens":%s}`, tc.value)
				if nested {
					line = fmt.Sprintf(`{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":2,"inputTokenDetails":{"cacheReadTokens":4,"cacheWriteTokens":%s}}}`, tc.value)
				}
				ev, err := NewReader(strings.NewReader(line)).Next()
				if err != nil {
					t.Fatalf("optional counter discarded the event: %v", err)
				}
				if nested {
					if ev.TotalUsage == nil || ev.TotalUsage.InputTokens != 10 || ev.TotalUsage.OutputTokens != 2 || ev.TotalUsage.CachedInputTokens != 4 || ev.TotalUsage.CacheWriteTokens != tc.want {
						t.Fatalf("usage = %+v", ev.TotalUsage)
					}
				} else if got := ev.CacheWriteTokens; got.Value != tc.want || got.Valid != tc.valid {
					t.Fatalf("counter = %+v, want value=%d valid=%t", got, tc.want, tc.valid)
				}
			}
		})
	}
}
