package commandcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event type markers that can appear inside the NDJSON stream and signal
// terminal (billing/plan) failures. Detected from command-code@1.32.2 and
// revalidated against command-code@1.51.3.
const (
	MarkerPremiumCreditsExhausted = "premium_credits_exhausted"
	MarkerModelNotInPlan          = "model_not_in_plan"
	MarkerInsufficientCredits     = "insufficient credits"
)

// APIError is an upstream non-2xx failure, shaped after the observed
// {"success":false,"error":{"code","status","message","docs"}} envelope.
type APIError struct {
	Status     int
	Code       string
	Type       string
	Message    string
	Param      string
	RetryAfter time.Duration
}

// HasCode accepts both gateway error codes and provider error types.
func (e *APIError) HasCode(code string) bool {
	return strings.EqualFold(e.Code, code) || strings.EqualFold(e.Type, code)
}

func (e *APIError) Error() string {
	return fmt.Sprintf("upstream %d %s: %s", e.Status, e.Code, e.Message)
}

// IsInsufficientCredits recognizes both the CLI's 400 signature and the
// Provider API's payment-required status or structured credit error.
func (e *APIError) IsInsufficientCredits() bool {
	return e.Status == 402 || e.HasCode("insufficient_credits") ||
		(e.Status == 400 && strings.Contains(strings.ToLower(e.Message), MarkerInsufficientCredits))
}

// IsRateLimited reports 429.
func (e *APIError) IsRateLimited() bool { return e.Status == 429 }

// IsSpendCapExceeded identifies the organization/model spend cap added in
// CLI 1.49.1. It is a policy limit, not a rejected credential.
func (e *APIError) IsSpendCapExceeded() bool { return e.HasCode("USAGE_EXCEEDED") }

// IsZDRUnavailable identifies a routing-policy failure, not a rejected key.
func (e *APIError) IsZDRUnavailable() bool {
	return strings.EqualFold(e.Code, "CMD_ZDR_NO_PROVIDERS") ||
		strings.EqualFold(e.Type, "cmd_zdr_no_providers")
}

// IsModelNotInPlan reports the 403 + "MODEL_NOT_IN_PLAN" signature: the key
// itself is valid, only the requested model is above the account's tier.
// It must NOT be treated as an auth rejection (which would circuit the key).
func (e *APIError) IsModelNotInPlan() bool {
	return e.HasCode("upgrade_required") || e.HasCode("MODEL_NOT_IN_PLAN") || e.Status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(e.Message), MarkerModelNotInPlan)
}

// IsModelNotRecognized reports the 403 + "Model/provider not recognized"
// signature: the model ID itself is unknown upstream (bare names get the
// default "anthropic:" provider prefix). A request-shape problem — no key
// in the pool will serve it, so it must neither circuit nor rotate keys.
func (e *APIError) IsModelNotRecognized() bool {
	return e.HasCode("unsupported_model") || e.Status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(e.Message), "model/provider not recognized")
}

// CallMeta carries per-call disguise metadata that travels from the server
// handler through the key pool into Credentials.
type CallMeta struct {
	// Root identifies the conversation (see convert.ConversationRoot) for
	// session identity derivation.
	Root string
	// TraceID pins the W3C trace ID across retry attempts (fresh span ID
	// per attempt), mirroring the CLI's per-iteration trace. Empty → fully
	// random traceparent per attempt.
	TraceID string
}

// Credentials identifies one upstream account plus per-request disguise headers.
type Credentials struct {
	APIKey      string
	SessionID   string
	ProjectSlug string
	// TraceID pins the W3C trace ID for this request so retry attempts
	// share it (fresh span ID per attempt), mirroring CLI iterations.
	// Empty → fully random traceparent per call.
	TraceID string
}

// Client talks to the Command Code API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	version    func() string // returns x-command-code-version value
	zdr        bool
	onKeyUse   func(key, version string) (finish func())
}

// NewClient builds a client. version is consulted per request so a
// background refresher can keep it current.
func NewClient(baseURL string, version func() string, zdr bool, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		httpClient: hc,
		version:    version,
		zdr:        zdr,
	}
}

// SetKeyObserver installs a nonblocking observer for accepted inference requests.
// Configure it before sharing the client with request handlers.
func (c *Client) SetKeyObserver(observer func(key, version string) func()) {
	c.onKeyUse = observer
}

func (c *Client) doInference(req *http.Request, apiKey string) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && req.Context().Err() == nil && c.onKeyUse != nil && apiKey != "" {
		if finish := c.onKeyUse(apiKey, req.Header.Get("x-command-code-version")); finish != nil {
			resp.Body = &activityBody{ReadCloser: resp.Body, finish: finish}
		}
	}
	return resp, err
}

// Keep the logical client active for the complete stream/JSON read. EOF,
// transport errors, and early Close all release activity exactly once.
type activityBody struct {
	io.ReadCloser
	once   sync.Once
	finish func()
}

func (b *activityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.finish)
	}
	return n, err
}

func (b *activityBody) Close() error {
	defer b.once.Do(b.finish)
	return b.ReadCloser.Close()
}

// Generate posts a GenerateRequest and returns the raw NDJSON response body.
// The caller must close it. Non-2xx responses become *APIError.
func (c *Client) Generate(ctx context.Context, creds Credentials, req *GenerateRequest) (io.ReadCloser, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal generate request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/alpha/generate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build generate request: %w", err)
	}
	for k, v := range c.headers(creds) {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.doInference(httpReq, creds.APIKey)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, parseAPIError(resp)
	}
	return resp.Body, nil
}

// RecordFingerprint posts the machine fingerprint to /alpha/fingerprint/record.
// The payload shape mirrors the real CLI exactly.
func (c *Client) RecordFingerprint(ctx context.Context, apiKey string, payload any, version string) error {
	return c.postJSON(ctx, "/alpha/fingerprint/record", apiKey, payload, version)
}

// RecordCLISession uses one version snapshot for both the event and headers.
// Its telemetry session is independent of inference conversation identities.
func (c *Client) RecordCLISession(ctx context.Context, apiKey, sessionID, platform, version string) error {
	payload := map[string]any{
		"eventType": "cli_session_exists",
		"metadata": map[string]string{
			"sessionId": sessionID, "cliVersion": version,
			"mode": "non-interactive", "os": platform,
		},
	}
	return c.postJSON(ctx, "/alpha/lifecycle-events", apiKey, payload, version)
}

// postJSON is the shared helper for fire-and-forget telemetry endpoints.
// Failures are returned but callers are expected to log-and-continue,
// matching the real CLI's silent-failure behavior.
func (c *Client) postJSON(ctx context.Context, route, apiKey string, payload any, version string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", route, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+route, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", route, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range c.authHeadersAtVersion(apiKey, version) {
		req.Header.Set(name, value)
	}

	// Do not observe telemetry traffic: doing so would recursively enqueue it.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s request: %w", route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp)
	}
	return nil
}

// setAuthHeaders applies the shared authenticated-CLI header set.
func (c *Client) setAuthHeaders(req *http.Request, apiKey string) {
	for name, value := range c.authHeaders(apiKey) {
		req.Header.Set(name, value)
	}
}

func (c *Client) authHeaders(apiKey string) map[string]string {
	return c.authHeadersAtVersion(apiKey, c.version())
}

func (c *Client) authHeadersAtVersion(apiKey, version string) map[string]string {
	headers := map[string]string{
		"Authorization":          "Bearer " + apiKey,
		"User-Agent":             "cli",
		"x-command-code-version": version,
		"x-cli-environment":      "production",
	}
	if c.zdr {
		headers["x-cmd-zdr"] = "1"
	}
	return headers
}

// Whoami probes /alpha/whoami — a lightweight auth/upstream health check.
func (c *Client) Whoami(ctx context.Context, apiKey string) error {
	return c.getJSON(ctx, "/alpha/whoami", apiKey, nil)
}

// Billing fetches /alpha/billing/credits and /alpha/billing/subscriptions,
// returning both raw payloads for downstream passthrough.
func (c *Client) Billing(ctx context.Context, apiKey string) (credits, subscriptions json.RawMessage, err error) {
	if err = c.getJSON(ctx, "/alpha/billing/credits", apiKey, &credits); err != nil {
		return nil, nil, fmt.Errorf("billing credits: %w", err)
	}
	if err = c.getJSON(ctx, "/alpha/billing/subscriptions", apiKey, &subscriptions); err != nil {
		return nil, nil, fmt.Errorf("billing subscriptions: %w", err)
	}
	return credits, subscriptions, nil
}

// getJSON performs an authenticated GET. When out is non-nil the response
// body is decoded into it; otherwise only the status is checked.
func (c *Client) getJSON(ctx context.Context, route, apiKey string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+route, nil)
	if err != nil {
		return fmt.Errorf("build %s request: %w", route, err)
	}
	c.setAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s request: %w", route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp)
	}
	if out != nil {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return fmt.Errorf("read %s response: %w", route, err)
		}
		switch o := out.(type) {
		case *json.RawMessage:
			*o = raw
		default:
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("decode %s response: %w", route, err)
			}
		}
	}
	return nil
}

// ModelInfo is one entry of the upstream model catalog.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
}

// ProviderModels fetches the upstream model catalog from
// /provider/v1/models, preserving display name and context length.
func (c *Client) ProviderModels(ctx context.Context, apiKey string) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/provider/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	c.setAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}

	var out struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	models := out.Data[:0]
	for _, m := range out.Data {
		if m.ID != "" {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models response contained no models")
	}
	return models, nil
}

// headers reproduces the real CLI header set (without the old proxy's
// phantom x-co-flag). When creds carries a TraceID the traceparent keeps
// it with a fresh span ID — mirroring the CLI, where retries inside one
// iteration share the trace ID and only re-roll the span.
func (c *Client) headers(creds Credentials) map[string]string {
	traceparent := NewTraceparent()
	if creds.TraceID != "" {
		traceparent = traceparentFromTraceID(creds.TraceID)
	}
	headers := c.authHeaders(creds.APIKey)
	headers["Content-Type"] = "application/json"
	headers["x-taste-learning"] = "false"
	headers["x-session-id"] = creds.SessionID
	headers["x-project-slug"] = creds.ProjectSlug
	headers["traceparent"] = traceparent
	return headers
}

// parseAPIError decodes the upstream error envelope, tolerating plain text.
func parseAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ae := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw)),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}

	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Status  int    `json:"status"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		ae.Code = envelope.Error.Code
		ae.Type = envelope.Error.Type
		ae.Param = envelope.Error.Param
		if envelope.Error.Message != "" {
			ae.Message = envelope.Error.Message
		}
		if envelope.Error.Status >= 400 && envelope.Error.Status <= 599 {
			ae.Status = envelope.Error.Status
		}
	}
	if ae.Message == "" {
		ae.Message = http.StatusText(resp.StatusCode)
		if ae.Message == "" {
			ae.Message = fmt.Sprintf("Upstream HTTP %d", resp.StatusCode)
		}
	}
	return ae
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

// NewTraceparent returns a W3C traceparent header value (version 00, sampled).
func NewTraceparent() string {
	return traceparentFromTraceID(NewTraceID())
}

// NewTraceID returns a random 32-hex W3C trace ID.
func NewTraceID() string {
	var traceID [16]byte
	_, _ = rand.Read(traceID[:])
	return hex.EncodeToString(traceID[:])
}

// traceparentFromTraceID renders a traceparent for traceID with a FRESH
// span ID. The real CLI keeps the iteration's trace ID across retry
// attempts and regenerates only the span ID per chat span.
func traceparentFromTraceID(traceID string) string {
	var spanID [8]byte
	_, _ = rand.Read(spanID[:])
	return fmt.Sprintf("00-%s-%s-01", traceID, hex.EncodeToString(spanID[:]))
}

// ProjectSlug derives a slug from a fake working-directory path using the
// same rules as the real CLI (lowercase, non-alnum runs → single dash).
func ProjectSlug(fakePath string) string {
	s := strings.ToLower(fakePath)
	var b strings.Builder
	lastDash := true // trims leading dashes
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
