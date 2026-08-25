package server

import (
	"context"
	"net/http"
	"strings"
)

// ctxKey is the context key type for request-scoped values.
type ctxKey int

const ctxDownstreamKey ctxKey = iota

// downstreamKey extracts the authenticated downstream identity from ctx.
func downstreamKey(ctx context.Context) string {
	if v, ok := ctx.Value(ctxDownstreamKey).(string); ok {
		return v
	}
	return ""
}

// authMiddleware enforces the configured downstream auth mode.
//
//	managed:     Authorization: Bearer <PROXY_API_KEY>
//	passthrough: any non-empty Bearer key (forwarded upstream)
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if s.cfg.AuthMode == "managed" {
			if key == "" || key != s.cfg.ProxyAPIKey {
				writeError(w, http.StatusUnauthorized, "authentication_error",
					"Invalid or missing proxy API key. Send Authorization: Bearer <key>.", 0)
				return
			}
			// Managed mode: the proxy key must never travel upstream —
			// downstream identity stays empty, the pool key is used instead.
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxDownstreamKey, "")))
			return
		}
		// passthrough
		if key == "" {
			writeError(w, http.StatusUnauthorized, "authentication_error",
				"Missing API key. Send Authorization: Bearer <key>.", 0)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxDownstreamKey, key)))
	})
}

// bearerToken extracts the Bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}
