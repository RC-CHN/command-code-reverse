package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/keypool"
)

// openAIError is the OpenAI error envelope.
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// writeError sends an OpenAI-shaped error response. When retryAfter > 0 a
// Retry-After header (seconds) is attached so SDKs back off politely.
func writeError(w http.ResponseWriter, status int, typ, msg string, retryAfter int) {
	writeCodedError(w, status, typ, "", msg, retryAfter)
}

func writeCodedError(w http.ResponseWriter, status int, typ, code, msg string, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIError{Error: openAIErrorBody{Message: msg, Type: typ, Code: code}})
}

// writeUpstreamError maps an upstream failure to an OpenAI error response.
// Billing/plan failures get explicit semantics instead of the old proxy's
// misleading "empty response 429".
func writeUpstreamError(w http.ResponseWriter, err error) {
	var unavailable *keypool.UnavailableError
	if errors.As(err, &unavailable) {
		writeCodedError(w, http.StatusServiceUnavailable, "server_error", "keypool_unavailable",
			"No upstream credentials are currently available.", retryAfterSeconds(unavailable.RetryAfter))
		return
	}

	var ae *commandcode.APIError
	if !errors.As(err, &ae) {
		writeCodedError(w, http.StatusBadGateway, "server_error", "upstream_connection_error",
			"Upstream connection error: "+err.Error(), 10)
		return
	}
	status, body, retry := classifyUpstreamError(ae)
	writeCodedError(w, status, body.Type, body.Code, body.Message, retry)
}

// classifyUpstreamError is shared by HTTP and in-band stream failures.
func classifyUpstreamError(ae *commandcode.APIError) (int, openAIErrorBody, int) {
	status, typ, msg, code, retry := http.StatusBadGateway, "upstream_error", "Upstream error: "+ae.Message, "", 0
	switch {
	case ae.IsSpendCapExceeded():
		status, typ, msg, code = http.StatusForbidden, "spend_limit_error", ae.Message, "USAGE_EXCEEDED"
	case ae.IsInsufficientCredits():
		status, typ, msg = http.StatusPaymentRequired, "billing_error", "Upstream account has insufficient credits: "+ae.Message
	case ae.IsRateLimited():
		status, typ, msg, retry = http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit: "+ae.Message, 10
	case ae.IsModelNotInPlan():
		status, typ, msg = http.StatusForbidden, "plan_error", "The requested model is not included in the upstream account's plan: "+ae.Message
	case ae.IsModelNotRecognized():
		status, typ, msg = http.StatusNotFound, "invalid_request_error", "Upstream does not recognize the model: "+ae.Message
	case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
		typ, msg = "upstream_auth_error", "Upstream rejected the CC API key: "+ae.Message
	case ae.Status >= 500 && ae.Status <= 599:
		status, typ, msg, retry = ae.Status, "server_error", "Upstream server error: "+ae.Message, 10
	}
	return status, openAIErrorBody{Message: msg, Type: typ, Code: code}, retry
}

func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	return int((d + time.Second - 1) / time.Second)
}

// terminalErrorStatus maps an in-band terminal marker to (status, type, message).
func terminalErrorStatus(marker string) (int, string, string) {
	switch marker {
	case commandcode.MarkerPremiumCreditsExhausted:
		return http.StatusPaymentRequired, "billing_error",
			"Premium model quota exhausted on the upstream account. Switch to a standard model or top up credits."
	case commandcode.MarkerModelNotInPlan:
		return http.StatusForbidden, "plan_error",
			"The requested model is not included in the upstream account's plan."
	case commandcode.MarkerInsufficientCredits:
		return http.StatusPaymentRequired, "billing_error",
			"Upstream account has insufficient credits."
	default:
		return http.StatusBadGateway, "upstream_error", "Upstream stream error: " + marker
	}
}

// streamErrorChunk builds an OpenAI chunk carrying an error object (used
// when headers are already sent and only the SSE channel remains).
func streamErrorChunk(msg, typ string) string {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
	return "data: " + string(b) + "\n\n"
}
