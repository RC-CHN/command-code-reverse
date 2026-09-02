package version

import (
	"context"
	"errors"
	"testing"
)

func TestFallbackWhenNothingConfigured(t *testing.T) {
	tr := New("", "")
	if tr.String() != "1.40.1" {
		t.Errorf("String() = %q, want fallback 1.40.1", tr.String())
	}
}

func TestPinDisablesRefresh(t *testing.T) {
	tr := New("9.9.9", "")
	tr.fetchFn = func(ctx context.Context) (string, error) {
		return "1.0.0", nil
	}
	// Start is a no-op when pinned; call refresh indirectly via Start's early return.
	tr.Start(t.Context())
	if tr.String() != "9.9.9" {
		t.Errorf("String() = %q, want pinned 9.9.9", tr.String())
	}
}

func TestRefreshUpdatesValue(t *testing.T) {
	tr := New("", "1.0.0")
	tr.fetchFn = func(ctx context.Context) (string, error) {
		return "1.33.0", nil
	}
	tr.refresh(t.Context())
	if tr.String() != "1.33.0" {
		t.Errorf("String() = %q", tr.String())
	}
}

func TestRefreshFailureKeepsCurrent(t *testing.T) {
	tr := New("", "1.0.0")
	tr.fetchFn = func(ctx context.Context) (string, error) {
		return "", errors.New("network down")
	}
	tr.refresh(t.Context())
	if tr.String() != "1.0.0" {
		t.Errorf("String() = %q, want unchanged 1.0.0", tr.String())
	}
}
