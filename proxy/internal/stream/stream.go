// Package stream reads the upstream NDJSON event stream and translates it
// into OpenAI-compatible SSE. It also detects the terminal billing/plan
// error markers the upstream embeds in-band.
package stream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
)

// Event is one decoded NDJSON line from the upstream stream.
type Event struct {
	Type string `json:"type"`

	// text-delta / reasoning-delta
	Text string `json:"text,omitempty"`
	ID   string `json:"id,omitempty"`

	// tool-call
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`

	// finish / finish-step
	FinishReason    string `json:"finishReason,omitempty"`
	RawFinishReason string `json:"rawFinishReason,omitempty"`
	Usage           *Usage `json:"usage,omitempty"`
	TotalUsage      *Usage `json:"totalUsage,omitempty"`

	// cache-write-tokens: optional fallback for the final usage counter.
	CacheWriteTokens TokenCount `json:"cacheWriteTokens"`

	// finish-step / provider-metadata: upstream cost accounting.
	ProviderMetadata *ProviderMetadata `json:"providerMetadata,omitempty"`

	// error
	Error   *StreamError `json:"error,omitempty"`
	Message string       `json:"message,omitempty"`

	// raw line, retained for unknown-event logging
	Raw json.RawMessage `json:"-"`
}

// Usage mirrors the upstream token accounting shape. totalTokens is
// intentionally omitted: verified live to always equal
// inputTokens + outputTokens (which we expose OpenAI-side), and
// inputTokens already includes cachedInputTokens.
type Usage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	CachedInputTokens int `json:"cachedInputTokens"`
	ReasoningTokens   int `json:"reasoningTokens"`
	CacheWriteTokens  int `json:"-"` // internal accounting; distinct from cache reads
}

// UnmarshalJSON accepts both legacy flat counters and the nested AI SDK
// usage read by CLI 1.51.3. An explicit nested zero overrides a flat value.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type plain Usage
	var wire struct {
		plain
		InputTokenDetails *struct {
			CacheReadTokens  *int       `json:"cacheReadTokens"`
			CacheWriteTokens TokenCount `json:"cacheWriteTokens"`
		} `json:"inputTokenDetails"`
		OutputTokenDetails *struct {
			ReasoningTokens *int `json:"reasoningTokens"`
		} `json:"outputTokenDetails"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*u = Usage(wire.plain)
	if wire.InputTokenDetails != nil && wire.InputTokenDetails.CacheReadTokens != nil {
		u.CachedInputTokens = *wire.InputTokenDetails.CacheReadTokens
	}
	if wire.InputTokenDetails != nil {
		u.CacheWriteTokens = wire.InputTokenDetails.CacheWriteTokens.Value
	}
	if wire.OutputTokenDetails != nil && wire.OutputTokenDetails.ReasoningTokens != nil {
		u.ReasoningTokens = *wire.OutputTokenDetails.ReasoningTokens
	}
	return nil
}

// StreamError is the payload of an "error" event.
type StreamError struct {
	Message    string `json:"message"`
	StatusCode int    `json:"statusCode,omitempty"`
	Code       string `json:"code,omitempty"`
	Type       string `json:"type,omitempty"`
}

// UnmarshalJSON mirrors readStreamErrorEvent: error can be a string or object.
func (e *StreamError) UnmarshalJSON(data []byte) error {
	var message string
	if json.Unmarshal(data, &message) == nil {
		*e = StreamError{Message: message}
		return nil
	}
	type plain StreamError
	return json.Unmarshal(data, (*plain)(e))
}

// APIError extracts stream failures, including JSON envelopes embedded in
// the error message. Unknown failures become 502 rather than empty success.
func (e *Event) APIError() *commandcode.APIError {
	if e.Type != "error" {
		return nil
	}
	out := &commandcode.APIError{Status: 502, Message: e.Message}
	if e.Error != nil {
		out.Code = e.Error.Code
		out.Type = e.Error.Type
		if e.Error.Message != "" {
			out.Message = e.Error.Message
		}
		if e.Error.StatusCode >= 400 && e.Error.StatusCode <= 599 {
			out.Status = e.Error.StatusCode
		}
	}
	if i := strings.IndexByte(out.Message, '{'); i >= 0 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Type    string `json:"type"`
				Message string `json:"message"`
				Status  int    `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(out.Message[i:]), &envelope) == nil {
			prefix := strings.Trim(strings.TrimSpace(out.Message[:i]), "<>")
			status, _ := strconv.Atoi(prefix)
			if envelope.Error.Status >= 400 && envelope.Error.Status <= 599 {
				status = envelope.Error.Status
			}
			if status >= 400 && status <= 599 {
				out.Status = status
			}
			if envelope.Error.Code != "" {
				out.Code = envelope.Error.Code
			}
			if envelope.Error.Type != "" {
				out.Type = envelope.Error.Type
			}
			if envelope.Error.Message != "" {
				out.Message = envelope.Error.Message
			}
		}
	}
	if out.Message == "" {
		out.Message = "Upstream stream error"
	}
	return out
}

// ProviderMetadata carries the gateway cost/routing info leaked in
// finish-step and provider-metadata events. Stripped before relaying
// downstream; only surfaced via logs/metrics.
type ProviderMetadata struct {
	Gateway *struct {
		Cost string `json:"cost"`
	} `json:"gateway,omitempty"`
}

// CostUSD parses the gateway cost string; empty/unparseable → 0.
func (p *ProviderMetadata) CostUSD() float64 {
	f, _ := p.ReportedCostUSD()
	return f
}

// ReportedCostUSD distinguishes an explicit zero from absent/invalid cost.
func (p *ProviderMetadata) ReportedCostUSD() (float64, bool) {
	if p == nil || p.Gateway == nil || p.Gateway.Cost == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(p.Gateway.Cost, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return f, true
}

// ErrMalformedEvent contains no upstream fields or decoder details.
var ErrMalformedEvent = errors.New("malformed upstream protocol event")

// Reader incrementally decodes an NDJSON event stream.
type Reader struct {
	br  *bufio.Reader
	err error // returned after any complete event read alongside it
}

// NewReader wraps an upstream response body. ReadString has no per-line
// size limit: start-step events can echo multi-megabyte image requests.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next event, or io.EOF at clean stream end.
// Blank lines and [DONE] sentinels are skipped.
func (r *Reader) Next() (*Event, error) {
	for {
		if r.err != nil {
			return nil, r.err
		}
		line, err := r.br.ReadString('\n')
		// A reader may return data and an error together. Decode any complete
		// final event before exposing the terminal error on the next call.
		r.err = err
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" || strings.HasPrefix(line, ":") {
			continue
		}
		var ev Event
		if json.Unmarshal([]byte(line), &ev) != nil {
			if malformedCriticalEvent(line) {
				r.err = ErrMalformedEvent
				return nil, r.err
			}
			continue // preserve tolerance for unrelated noise/unknown events
		}
		if ev.Type == "" {
			continue
		}
		ev.Raw = json.RawMessage(line)
		return &ev, nil
	}
}

// malformedCriticalEvent reads only enough JSON to identify a tool/terminal
// event. This also catches a readable type followed by a truncated payload;
// arbitrary text and unrelated events keep the reader's previous tolerance.
func malformedCriticalEvent(line string) bool {
	d := json.NewDecoder(strings.NewReader(line))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return false
		}
		if key == "type" {
			var kind string
			if d.Decode(&kind) != nil {
				return false
			}
			switch kind {
			case "tool-call", "tool-input-start", "tool-input-delta", "tool-input-end", "finish", "finish-step", "abort", "error":
				return true
			}
		} else {
			var value json.RawMessage
			if d.Decode(&value) != nil {
				return false
			}
		}
	}
	return false
}

// TerminalError inspects an event for the in-band terminal markers
// (premium_credits_exhausted / model_not_in_plan / insufficient credits).
// Returns nil when the event carries no terminal marker.
func TerminalError(ev *Event) *StreamError {
	if ev.Type != "error" {
		return nil
	}
	msg := ""
	if ev.Error != nil {
		msg = ev.Error.Message
	}
	lower := strings.ToLower(msg)
	// The markers may appear as the whole message or embedded in JSON/text.
	for _, marker := range []string{
		commandcode.MarkerPremiumCreditsExhausted,
		commandcode.MarkerModelNotInPlan,
		commandcode.MarkerInsufficientCredits,
	} {
		if strings.Contains(lower, marker) {
			return &StreamError{Message: marker}
		}
	}
	// Also scan the raw line: markers can ride inside provider error payloads.
	lower = strings.ToLower(string(ev.Raw))
	for _, marker := range []string{
		commandcode.MarkerPremiumCreditsExhausted,
		commandcode.MarkerModelNotInPlan,
		commandcode.MarkerInsufficientCredits,
	} {
		if strings.Contains(lower, marker) {
			return &StreamError{Message: marker}
		}
	}
	return nil
}

// OpenAIFinishReason follows the CLI's consumedStopReason: either finish field
// can identify a refusal, which maps to OpenAI's content_filter reason.
func (e *Event) OpenAIFinishReason() string {
	if isRefusalFinishReason(e.RawFinishReason) {
		return "content_filter"
	}
	return MapFinishReason(e.FinishReason)
}

func isRefusalFinishReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "refusal", "content-filter", "content_filter":
		return true
	default:
		return false
	}
}

// MapFinishReason converts upstream finish reasons to OpenAI values.
func MapFinishReason(reason string) string {
	if isRefusalFinishReason(reason) {
		return "content_filter"
	}
	switch reason {
	case "tool-calls", "tool_use", "tool_calls":
		return "tool_calls"
	case "length", "max_tokens":
		return "length"
	case "stop", "end_turn", "":
		return "stop"
	default:
		return "" // unknown/error reasons are not a successful stop
	}
}

// ── OpenAI SSE output ───────────────────────────────────────────────

// Chunk is one OpenAI chat.completion.chunk.
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *OpenAIUsage  `json:"usage,omitempty"`
}

// ChunkChoice is a single choice in a chunk.
type ChunkChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// OpenAIUsage is the OpenAI usage block with cache and reasoning details.
type OpenAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// UsageFromUpstream maps and normalizes upstream usage. The zero-output
// rule zeroes everything (anti false-billing, inherited from the old proxy).
func UsageFromUpstream(u *Usage) *OpenAIUsage {
	out := &OpenAIUsage{}
	if u == nil {
		return out
	}
	input, output, cached, reasoning := u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.ReasoningTokens
	if output == 0 {
		input, cached, reasoning = 0, 0, 0
	}
	out.PromptTokens = input
	out.CompletionTokens = output
	out.TotalTokens = input + output
	out.PromptTokensDetails.CachedTokens = cached
	out.CompletionTokensDetails.ReasoningTokens = reasoning
	return out
}

// SSE serializes a chunk as one SSE data frame.
func SSE(c *Chunk) string {
	b, _ := json.Marshal(c)
	return fmt.Sprintf("data: %s\n\n", b)
}

// DoneSSE is the terminal SSE sentinel.
const DoneSSE = "data: [DONE]\n\n"

// NewChunk builds a chunk with the invariant fields filled.
func NewChunk(id string, created int64, model string, delta map[string]any, finish *string, usage *OpenAIUsage) *Chunk {
	return &Chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		Usage:   usage,
	}
}
