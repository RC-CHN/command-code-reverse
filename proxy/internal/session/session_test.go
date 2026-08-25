package session

import (
	"strings"
	"testing"
	"time"
)

func TestSessionStableWithinWindow(t *testing.T) {
	s := NewStore()
	a := s.SessionID("k1")
	b := s.SessionID("k1")
	if a != b {
		t.Fatalf("session rotated early: %q vs %q", a, b)
	}
	if s.SessionID("k2") == a {
		t.Fatal("different keys must get different sessions")
	}
}

func TestSessionRotatesAfterExpiry(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.now = func() time.Time { return now }

	first := s.SessionID("k1")
	now = now.Add(13 * time.Hour) // beyond base duration + any jitter
	second := s.SessionID("k1")
	if first == second {
		t.Fatal("session should rotate after expiry")
	}
}

func TestProjectSlugShape(t *testing.T) {
	s := NewStore()
	slug := s.ProjectSlug("k1")
	if !strings.HasPrefix(slug, "c-users-dev-projects-") {
		t.Errorf("slug = %q", slug)
	}
	if slug != s.ProjectSlug("k1") {
		t.Error("slug should be deterministic within a session")
	}
}

func TestSweep(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	s.SessionID("k1")
	now = now.Add(13 * time.Hour)
	s.Sweep()
	if s.Len() != 0 {
		t.Fatalf("Len = %d after sweep", s.Len())
	}
}
