// Package stream reads the upstream NDJSON event stream and translates it
// into OpenAI-compatible SSE. It also detects the terminal billing/plan
// error markers the upstream embeds in-band.
package stream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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
	FinishReason string `json:"finishReason,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	TotalUsage   *Usage `json:"totalUsage,omitempty"`

	// finish-step / provider-metadata: upstream cost accounting.
	ProviderMetadata *ProviderMetadata `json:"providerMetadata,omitempty"`

	// error
	Error *StreamError `json:"error,omitempty"`

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
}

// StreamError is the payload of an "error" event.
type StreamError struct {
	Message    string `json:"message"`
	StatusCode int    `json:"statusCode,omitempty"`
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
	if p == nil || p.Gateway == nil || p.Gateway.Cost == "" {
		return 0
	}
	var f float64
	if _, err := fmt.Sscanf(p.Gateway.Cost, "%g", &f); err != nil {
		return 0
	}
	return f
}

// Reader incrementally decodes an NDJSON event stream.
type Reader struct {
	sc *bufio.Scanner
}

// NewReader wraps an upstream response body. The buffer is sized for
// start-step events, which embed the full forwarded request.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &Reader{sc: sc}
}

// Next returns the next event, or io.EOF at clean stream end.
// Blank lines and [DONE] sentinels are skipped.
func (r *Reader) Next() (*Event, error) {
	for r.sc.Scan() {
		line := strings.TrimSpace(r.sc.Text())
		if line == "" || line == "[DONE]" || strings.HasPrefix(line, ":") {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // tolerate malformed lines
		}
		if ev.Type == "" {
			continue
		}
		ev.Raw = json.RawMessage(line)
		return &ev, nil
	}
	if err := r.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
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

// MapFinishReason converts upstream finish reasons to OpenAI values.
func MapFinishReason(reason string) string {
	switch reason {
	case "tool-calls", "tool_use", "tool_calls":
		return "tool_calls"
	case "length", "max_tokens":
		return "length"
	case "stop", "end_turn", "":
		return "stop"
	default:
		return "stop"
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

// OpenAIUsage is the OpenAI usage block with cache details.
type OpenAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// UsageFromUpstream maps and normalizes upstream usage. The zero-output
// rule zeroes everything (anti false-billing, inherited from the old proxy).
func UsageFromUpstream(u *Usage) *OpenAIUsage {
	out := &OpenAIUsage{}
	if u == nil {
		return out
	}
	input, output, cached := u.InputTokens, u.OutputTokens, u.CachedInputTokens
	if output == 0 {
		input, cached = 0, 0
	}
	out.PromptTokens = input
	out.CompletionTokens = output
	out.TotalTokens = input + output
	out.PromptTokensDetails.CachedTokens = cached
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
