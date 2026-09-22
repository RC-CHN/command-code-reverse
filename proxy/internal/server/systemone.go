package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
)

type SystemOneUpstream interface {
	SystemOne(context.Context, string, commandcode.CallMeta, *commandcode.SystemOneRequest) (*commandcode.SystemOneResponse, error)
}

func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			writeCodedError(w, 413, "invalid_request_error", "request_body_too_large", fmt.Sprintf("Request body exceeds MAX_BODY_BYTES (%d bytes)", sizeErr.Limit), 0)
		} else {
			writeCodedError(w, 400, "invalid_request_error", "invalid_json", "Could not read JSON request body", 0)
		}
		return
	}
	req, err := commandcode.ParseSystemOneRequest(raw, s.cfg.JevUnlockMaxOptions)
	if err != nil {
		var invalid *commandcode.RequestError
		if errors.As(err, &invalid) {
			writeErrorBody(w, invalid.Status, openAIErrorBody{Type: "invalid_request_error", Code: invalid.Code, Param: invalid.Param, Message: invalid.Message}, 0)
		} else {
			writeCodedError(w, 400, "invalid_request_error", "invalid_json", "Invalid System One request", 0)
		}
		return
	}
	if s.deps.SystemOne == nil {
		writeCodedError(w, 501, "not_implemented", "systemone_unavailable", "System One is not configured", 0)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.JevTimeout)
	defer cancel()
	// Jev is stateless: a fresh synthetic identity per request, stable only
	// across its key attempts. No state/question content enters identity logs.
	meta := commandcode.CallMeta{Root: "systemone:" + commandcode.NewTraceID(), TraceID: commandcode.NewTraceID()}
	response, err := s.deps.SystemOne.SystemOne(ctx, downstreamKey(r.Context()), meta, req)
	result := "ok"
	if err != nil {
		result = "upstream_error"
		if r.Context().Err() != nil {
			result = "canceled"
		} else {
			writeSystemOneError(w, err)
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response.Body)
	}
	if s.deps.Metrics != nil {
		s.deps.Metrics.IncRequest(req.Model, false, result)
		s.deps.Metrics.ObserveLatency(req.Model, false, time.Since(start).Milliseconds())
		if response != nil && response.Usage != nil && err == nil {
			s.deps.Metrics.AddTokens("input", response.Usage.InputTokens)
			s.deps.Metrics.AddTokens("output", response.Usage.OutputTokens)
		}
	}
	// Upstream error bodies can quote state; never log those bodies here.
	var ae *commandcode.APIError
	code := ""
	if errors.As(err, &ae) {
		code = ae.Code
	}
	slog.Info("systemone request served", "model", req.Model, "result", result, "errorCode", code, "elapsedMs", time.Since(start).Milliseconds())
}

func writeSystemOneError(w http.ResponseWriter, err error) {
	var unavailable *keypool.UnavailableError
	if errors.As(err, &unavailable) {
		writeCodedError(w, 503, "server_error", "keypool_unavailable", "No Jev upstream credentials are currently available", retryAfterSeconds(unavailable.RetryAfter))
		return
	}
	var transport net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &transport) && transport.Timeout() {
		writeCodedError(w, 504, "server_error", "upstream_timeout", "System One request timed out", 0)
		return
	}
	var ae *commandcode.APIError
	if !errors.As(err, &ae) {
		writeCodedError(w, 502, "server_error", "upstream_connection_error", "System One upstream connection failed", 0)
		return
	}
	status, code, retry := ae.Status, ae.Code, 0
	var typ string
	if code == "" {
		code = ae.Type
	}
	switch {
	case ae.IsZDRUnavailable():
		status, typ, code = 403, "zdr_error", "CMD_ZDR_NO_PROVIDERS"
	case ae.IsSpendCapExceeded():
		status, typ, code = 403, "spend_limit_error", "USAGE_EXCEEDED"
	case ae.IsInsufficientCredits():
		status, typ = 402, "billing_error"
	case ae.IsModelNotInPlan():
		status, typ = 403, "plan_error"
	case ae.Status == 401 || ae.Status == 403 && (ae.HasCode("authentication_error") || ae.HasCode("invalid_api_key")):
		status, typ = 502, "upstream_auth_error"
	case ae.Status == 403:
		typ = "permission_error"
	case ae.IsRateLimited():
		typ, retry = "rate_limit_error", 10
	case ae.Status >= 400 && ae.Status < 500:
		typ = "invalid_request_error"
	case ae.Status >= 500 && ae.Status <= 599:
		typ = "server_error"
		if ae.Status == 503 || ae.Status == 529 {
			retry = 10
		}
	default:
		status, typ = 502, "upstream_error"
	}
	if code == "" {
		code = typ
	}
	if (ae.IsRateLimited() || ae.Status == 503 || ae.Status == 529) && retry > 0 && ae.RetryAfter > 0 {
		retry = retryAfterSeconds(ae.RetryAfter)
	}
	writeErrorBody(w, status, openAIErrorBody{Message: ae.Message, Type: typ, Code: code, Param: ae.Param}, retry)
}
