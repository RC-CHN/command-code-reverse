package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
)

func TestWriteUpstreamErrorKeypoolUnavailable(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUpstreamError(rec, &keypool.UnavailableError{KeyCount: 2, RetryAfter: 45*time.Second + time.Millisecond})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "46" {
		t.Fatalf("Retry-After = %q, want 46", got)
	}
	assertErrorBody(t, rec, "server_error", "keypool_unavailable")
}

func TestWriteUpstreamErrorPreservesServerStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUpstreamError(rec, &commandcode.APIError{Status: http.StatusInternalServerError, Message: "internal server error"})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want upstream 500", rec.Code)
	}
	assertErrorBody(t, rec, "server_error", "")
}

func TestWriteUpstreamErrorTransportFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUpstreamError(rec, errors.New("connection reset"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	assertErrorBody(t, rec, "server_error", "upstream_connection_error")
}

func assertErrorBody(t *testing.T, rec *httptest.ResponseRecorder, wantType, wantCode string) {
	t.Helper()
	var got openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Error.Type != wantType || got.Error.Code != wantCode {
		t.Fatalf("error body = %+v, want type=%q code=%q", got.Error, wantType, wantCode)
	}
}
