package stream

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func BenchmarkReader(b *testing.B) {
	for _, tc := range []struct{ name, data string }{
		{"deltas", strings.Repeat(`{"type":"text-delta","text":"0123456789abcdef0123456789abcdef"}`+"\n", 2048)},
		{"image_echo", `{"type":"start-step","request":{"image":"` + strings.Repeat("x", 5<<20) + `"}}` + "\n"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.data)))
			for range b.N {
				r := NewReader(strings.NewReader(tc.data))
				for {
					_, err := r.Next()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
