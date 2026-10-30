package stats

import (
	"context"
	"strconv"
	"strings"
)

type selectionContextKey struct{}

// KeySelection observes routing without enabling dashboard retention. Like
// Request, it belongs to the handler goroutine; it stores no credential bytes
// or attempt history and cannot alter pool selection or circuit breakers.
type KeySelection struct {
	ID       string
	Attempts int
	keyCount int
}

func ObserveKeys(ctx context.Context, keyCount int) (context.Context, *KeySelection) {
	selection := &KeySelection{keyCount: keyCount}
	return context.WithValue(ctx, selectionContextKey{}, selection), selection
}

func observeKey(ctx context.Context, id string) {
	selection, _ := ctx.Value(selectionContextKey{}).(*KeySelection)
	if selection == nil {
		return
	}
	if id != "passthrough" {
		index, err := strconv.Atoi(strings.TrimPrefix(id, "key-"))
		if err != nil || index < 1 || index > selection.keyCount || id != KeyID(index-1) {
			return
		}
	}
	selection.ID = id
	selection.Attempts++
}
