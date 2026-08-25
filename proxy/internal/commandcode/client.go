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
	"strings"
)

// Event type markers that can appear inside the NDJSON stream and signal
// terminal (billing/plan) failures. Detected from command-code@1.32.2.
const (
	MarkerPremiumCreditsExhausted = "premium_credits_exhausted"
	MarkerModelNotInPlan          = "model_not_in_plan"
	MarkerInsufficientCredits     = "insufficient credits"
)

// APIError is an upstream non-2xx failure, shaped after the observed
// {"success":false,"error":{"code","status","message","docs"}} envelope.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("upstream %d %s: %s", e.Status, e.Code, e.Message)
}

// IsInsufficientCredits reports the 400 + "insufficient credits" signature.
func (e *APIError) IsInsufficientCredits() bool {
	return e.Status == 400 && strings.Contains(strings.ToLower(e.Message), MarkerInsufficientCredits)
}

// IsRateLimited reports 429.
func (e *APIError) IsRateLimited() bool { return e.Status == 429 }

// Credentials identifies one upstream account plus per-request disguise headers.
type Credentials struct {
	APIKey      string
	SessionID   string
	ProjectSlug string
}

// Client talks to the Command Code API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	version    func() string // returns x-command-code-version value
}

// NewClient builds a client. version is consulted per request so a
// background refresher can keep it current.
func NewClient(baseURL string, version func() string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		httpClient: hc,
		version:    version,
	}
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

	resp, err := c.httpClient.Do(httpReq)
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
func (c *Client) RecordFingerprint(ctx context.Context, apiKey string, payload any) error {
	return c.postJSON(ctx, "/alpha/fingerprint/record", apiKey, payload)
}

// RecordLifecycleEvent posts a lifecycle event to /alpha/lifecycle-events.
func (c *Client) RecordLifecycleEvent(ctx context.Context, apiKey string, payload any) error {
	return c.postJSON(ctx, "/alpha/lifecycle-events", apiKey, payload)
}

// postJSON is the shared helper for fire-and-forget telemetry endpoints.
// Failures are returned but callers are expected to log-and-continue,
// matching the real CLI's silent-failure behavior.
func (c *Client) postJSON(ctx context.Context, route, apiKey string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", route, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+route, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", route, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "cli")
	req.Header.Set("x-command-code-version", c.version())
	req.Header.Set("x-cli-environment", "production")

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
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "cli")
	req.Header.Set("x-command-code-version", c.version())
	req.Header.Set("x-cli-environment", "production")

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

// ProviderModels fetches the upstream model catalog from
// /provider/v1/models and returns the model IDs.
func (c *Client) ProviderModels(ctx context.Context, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/provider/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-command-code-version", c.version())
	req.Header.Set("x-cli-environment", "production")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("models response contained no models")
	}
	return ids, nil
}

// headers reproduces the real CLI header set (without the old proxy's
// phantom x-co-flag).
func (c *Client) headers(creds Credentials) map[string]string {
	return map[string]string{
		"Content-Type":           "application/json",
		"Authorization":          "Bearer " + creds.APIKey,
		"User-Agent":             "cli",
		"x-command-code-version": c.version(),
		"x-cli-environment":      "production",
		"x-taste-learning":       "false",
		"x-session-id":           creds.SessionID,
		"x-project-slug":         creds.ProjectSlug,
		"traceparent":            NewTraceparent(),
	}
}

// parseAPIError decodes the upstream error envelope, tolerating plain text.
func parseAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ae := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}

	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Status  int    `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Message != "" {
		ae.Code = envelope.Error.Code
		ae.Message = envelope.Error.Message
		if envelope.Error.Status != 0 {
			ae.Status = envelope.Error.Status
		}
	}
	return ae
}

// NewTraceparent returns a W3C traceparent header value (version 00, sampled).
func NewTraceparent() string {
	var traceID, spanID [16]byte
	_, _ = rand.Read(traceID[:])
	_, _ = rand.Read(spanID[:8])
	return fmt.Sprintf("00-%s-%s-01", hex.EncodeToString(traceID[:]), hex.EncodeToString(spanID[:8]))
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
