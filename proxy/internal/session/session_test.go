package session

import (
	"regexp"
	"testing"
)

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDeterministicPerConversation(t *testing.T) {
	s := NewStore("test-secret")
	a := s.SessionID("k1", "rootA")
	if a != s.SessionID("k1", "rootA") {
		t.Error("same key+root must derive the same session ID")
	}
	if !uuidShape.MatchString(a) {
		t.Errorf("session ID %q is not UUID-v4-shaped", a)
	}
	if s.SessionID("k1", "rootB") == a {
		t.Error("different conversation root must derive a different session ID")
	}
	if s.SessionID("k2", "rootA") == a {
		t.Error("different key must derive a different session ID (account scope)")
	}
}

func TestSecretChangesIdentity(t *testing.T) {
	a := NewStore("secret-1").SessionID("k1", "rootA")
	b := NewStore("secret-2").SessionID("k1", "rootA")
	if a == b {
		t.Error("different secrets must derive different identities")
	}
}

func TestThreadIDIsConversationScoped(t *testing.T) {
	s := NewStore("test-secret")
	tid := s.ThreadID("rootA")
	if !uuidShape.MatchString(tid) {
		t.Errorf("thread ID %q is not UUID-v4-shaped", tid)
	}
	if tid != s.ThreadID("rootA") {
		t.Error("thread ID must be stable within a conversation")
	}
	if tid == s.ThreadID("rootB") {
		t.Error("thread ID must differ across conversations")
	}
	// Thread scope excludes the key: identity survives a mid-conversation spill.
	if tid == s.SessionID("k1", "rootA") {
		t.Error("thread and session scopes must not collide")
	}
}

func TestProjectSlugShape(t *testing.T) {
	s := NewStore("test-secret")
	slug := s.ProjectSlug("k1", "rootA")
	if slug == "" || slug != s.ProjectSlug("k1", "rootA") {
		t.Fatalf("slug must be non-empty and deterministic, got %q", slug)
	}
	if slug == s.ProjectSlug("k1", "rootB") {
		t.Error("slug should differ across conversations")
	}
}

func TestEmptySecretRandomized(t *testing.T) {
	a := NewStore("").SessionID("k1", "rootA")
	b := NewStore("").SessionID("k1", "rootA")
	if a == b {
		t.Error("empty secret must randomize per store (per-boot identity)")
	}
}
