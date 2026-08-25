// Package ids generates random identifiers (UUIDs, completion IDs)
// without third-party dependencies.
package ids

import (
	"crypto/rand"
	"fmt"
)

// NewUUID returns a random RFC 4122 version 4 UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewCompletionID returns an OpenAI-style chat completion ID.
func NewCompletionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("chatcmpl-%x", b)
}
