// Package session maintains per-upstream-key session IDs with rotation,
// mirroring the old proxy's 12h + 1h jitter scheme. Sessions are pure
// disguise metadata — the upstream does not validate continuity, so an
// in-memory store is sufficient (see design doc §8).
package session

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/ids"
)

const (
	// duration is the base session lifetime.
	duration = 12 * time.Hour
	// jitterMax bounds the random additive jitter.
	jitterMax = time.Hour
)

// projectNames feeds fake project paths for slug generation (same pool the
// old proxy used — keeps the slug shape indistinguishable).
var projectNames = []string{
	"app", "api", "backend", "bot", "cli", "core", "data", "frontend",
	"lib", "plugin", "proxy", "server", "service", "tool", "web", "worker",
}

type entry struct {
	sessionID string
	expiresAt time.Time
}

// Store hands out stable session IDs per key, rotating on expiry.
type Store struct {
	mu  sync.Mutex
	m   map[string]entry
	now func() time.Time // test hook
}

// NewStore builds an empty store.
func NewStore() *Store {
	return &Store{m: map[string]entry{}, now: time.Now}
}

// SessionID returns the live session ID for key, creating or rotating as
// needed.
func (s *Store) SessionID(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.m[key]; ok && s.now().Before(e.expiresAt) {
		return e.sessionID
	}
	e := entry{
		sessionID: ids.NewUUID(),
		expiresAt: s.now().Add(duration + jitter()),
	}
	s.m[key] = e
	return e.sessionID
}

// ProjectSlug derives a deterministic fake project slug from the key's
// current session ID (shape-compatible with the real CLI's slugs).
func (s *Store) ProjectSlug(key string) string {
	sid := s.SessionID(key)
	var n int
	for _, c := range sid[:4] {
		n = n*16 + int(c)
	}
	name := projectNames[n%len(projectNames)]
	fakePath := fmt.Sprintf(`C:\Users\dev\projects\%s-%s`, name, sid[:4])
	return commandcode.ProjectSlug(fakePath)
}

// Sweep drops expired entries; call periodically to bound memory.
func (s *Store) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.m {
		if !s.now().Before(e.expiresAt) {
			delete(s.m, k)
		}
	}
}

// Len reports the number of tracked sessions (testing/metrics).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// jitter returns a uniform random duration in [0, jitterMax).
func jitter() time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(jitterMax)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}
