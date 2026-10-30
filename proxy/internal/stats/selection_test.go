package stats

import (
	"context"
	"testing"
)

func TestKeyObservationWithoutRetention(t *testing.T) {
	ctx, selection := ObserveKeys(context.Background(), 80)
	for i := range 80 {
		SelectKey(ctx, KeyID(i))
	}
	for _, id := range []string{"PRIVATE_KEY", "key-0", "key-81", "key-01", "1"} {
		SelectKey(ctx, id)
	}
	if selection.ID != "key-80" || selection.Attempts != 80 || FromContext(ctx) != nil {
		t.Fatalf("incorrect/unbounded observation: %+v", selection)
	}
	SelectKey(ctx, "passthrough")
	if selection.ID != "passthrough" || selection.Attempts != 81 {
		t.Fatalf("passthrough: %+v", selection)
	}
}
