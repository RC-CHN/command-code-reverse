// Package session derives upstream session identity from the conversation
// root instead of wall-clock rotation. The upstream can reconstruct
// conversations by prefix-matching message histories, so session identity
// must be stable within one conversation and fresh across conversations —
// timers get both wrong. Derivation is stateless: HMAC(secret, parts) with
// the secret coming from FINGERPRINT_SEED (or random per boot, which is
// itself CLI-like: a restarted CLI opens new sessions).
//
// Two scopes (design: thread = conversation, session = account login):
//   - ThreadID(root)       — stable across key spills mid-conversation
//   - SessionID(key, root) — per account; a spill plausibly looks like a
//     fresh login continuing an old conversation
package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
)

// projectNames feeds fake project paths for slug generation (same pool the
// old proxy used — keeps the slug shape indistinguishable).
var projectNames = []string{
	"app", "api", "backend", "bot", "cli", "core", "data", "frontend",
	"lib", "plugin", "proxy", "server", "service", "tool", "web", "worker",
}

// Store derives deterministic identities. Immutable after construction.
type Store struct {
	secret []byte
}

// NewStore builds a store from a hex/plain secret; empty → random per boot.
func NewStore(secret string) *Store {
	if secret == "" {
		secret = hex.EncodeToString(randomBytes(32))
	}
	return &Store{secret: []byte(secret)}
}

// ThreadID returns the conversation-scoped thread ID (valid UUID shape,
// like the CLI's crypto.randomUUID).
func (s *Store) ThreadID(root string) string {
	return s.uuid("thread", "", root)
}

// SessionID returns the account-scoped session ID (valid UUID shape).
func (s *Store) SessionID(key, root string) string {
	return s.uuid("session", key, root)
}

// ProjectPath derives a stable Linux workspace path, matching the proxy's
// request environment. Headers and request config must describe this same path.
func (s *Store) ProjectPath(key, root string) string {
	sid := s.SessionID(key, root)
	n, _ := strconv.ParseUint(sid[:4], 16, 16)
	name := projectNames[n%uint64(len(projectNames))]
	return fmt.Sprintf("/home/dev/projects/%s-%s", name, sid[:4])
}

// ProjectSlug uses the same workspace path sent in config.workingDir.
func (s *Store) ProjectSlug(key, root string) string {
	return commandcode.ProjectSlug(s.ProjectPath(key, root))
}

// uuid renders HMAC(secret, label\0key\0root) as a version-4-shaped UUID.
func (s *Store) uuid(label, key, root string) string {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(label))
	h.Write([]byte{0})
	h.Write([]byte(key))
	h.Write([]byte{0})
	h.Write([]byte(root))
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x40 // version 4
	sum[8] = (sum[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
