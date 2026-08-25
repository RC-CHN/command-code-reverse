// Package convert translates OpenAI chat completion requests into the
// Command Code wire format (and back, for responses). All functions are
// pure and table-tested.
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
)

// ── OpenAI request types (only the fields we honor) ─────────────────

// ChatRequest is the OpenAI /v1/chat/completions request shape.
type ChatRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Stream          bool            `json:"stream"`
	MaxTokens       *int            `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Tools           []Tool          `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
}

// Message is an OpenAI chat message. Content may be a string or an array
// of content parts; use ContentText / ContentParts to access.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// ToolCall is an OpenAI assistant tool call.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is an OpenAI function tool definition.
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ContentPart is one element of an OpenAI array-form content field.
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// ContentText extracts plain text from a message content field,
// tolerating both string and array forms.
func (m Message) ContentText() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []ContentPart
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ContentParts parses array-form content; returns nil for string form.
func (m Message) ContentParts() []ContentPart {
	var parts []ContentPart
	if len(m.Content) == 0 || json.Unmarshal(m.Content, &parts) != nil {
		return nil
	}
	return parts
}

// ── conversion ──────────────────────────────────────────────────────

// ToWire converts an OpenAI request into the upstream wire format.
// maxTokensClamp bounds max_tokens; defaultMaxTokens applies when unset.
func ToWire(req *ChatRequest, threadID string, maxTokensClamp, defaultMaxTokens int) (*commandcode.GenerateRequest, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("convert: model is required")
	}

	system, messages, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}

	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = min(*req.MaxTokens, maxTokensClamp)
	}

	out := &commandcode.GenerateRequest{
		Config: commandcode.RequestConfig{
			WorkingDir:    "/tmp/proxy",
			Date:          time.Now().UTC().Format("2006-01-02"),
			Environment:   "linux",
			Structure:     []string{},
			IsGitRepo:     false,
			CurrentBranch: "",
			MainBranch:    "",
			GitStatus:     "",
			RecentCommits: []string{},
		},
		Memory:         nil,
		Taste:          nil,
		Skills:         nil,
		PermissionMode: "standard", // wire value; proxy never executes tools server-side
		Mode:           "agent",
		ThreadID:       threadID, // caller guarantees valid UUID or empty
		Params: commandcode.Params{
			Model:           req.Model,
			Messages:        messages,
			System:          system,
			MaxTokens:       maxTokens,
			Stream:          true, // upstream is always streamed
			Temperature:     req.Temperature,
			ReasoningEffort: req.ReasoningEffort,
		},
	}

	if len(req.Tools) > 0 {
		out.Params.Tools = make([]commandcode.WireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var schema any
			if len(t.Function.Parameters) > 0 {
				if json.Unmarshal(t.Function.Parameters, &schema) != nil {
					schema = map[string]any{"type": "object", "properties": map[string]any{}}
				}
			} else {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out.Params.Tools = append(out.Params.Tools, commandcode.WireTool{
				Type:        "function",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: schema,
			})
		}
	}

	if tc, err := convertToolChoice(req.ToolChoice); err != nil {
		return nil, err
	} else if tc != nil {
		out.Params.ToolChoice = tc
	}

	return out, nil
}

// convertMessages splits OpenAI messages into a system prompt plus wire
// messages. System/developer messages are joined with newlines.
func convertMessages(msgs []Message) (string, []commandcode.WireMessage, error) {
	var sysParts []string
	var out []commandcode.WireMessage

	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			if t := m.ContentText(); t != "" {
				sysParts = append(sysParts, t)
			}

		case "user":
			parts := []commandcode.WireContentPart{}
			if raw := m.ContentParts(); raw != nil {
				for _, p := range raw {
					switch p.Type {
					case "text":
						parts = append(parts, commandcode.WireContentPart{Type: "text", Text: p.Text})
					case "image_url":
						if p.ImageURL != nil && p.ImageURL.URL != "" {
							mt := mimeFromDataURL(p.ImageURL.URL)
							parts = append(parts, commandcode.WireContentPart{Type: "image", Image: p.ImageURL.URL, MimeType: mt})
						}
					}
				}
			} else if t := m.ContentText(); t != "" {
				parts = append(parts, commandcode.WireContentPart{Type: "text", Text: t})
			}
			if len(parts) > 0 {
				out = append(out, commandcode.WireMessage{Role: "user", Content: parts})
			}

		case "assistant":
			parts := []commandcode.WireContentPart{}
			if t := m.ContentText(); t != "" {
				parts = append(parts, commandcode.WireContentPart{Type: "text", Text: t})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, commandcode.WireContentPart{
					Type:       "tool-call",
					ToolCallID: tc.ID,
					ToolName:   tc.Function.Name,
					Input:      parseArguments(tc.Function.Arguments),
				})
			}
			if len(parts) > 0 {
				out = append(out, commandcode.WireMessage{Role: "assistant", Content: parts})
			}

		case "tool":
			out = append(out, commandcode.WireMessage{
				Role: "tool",
				Content: []commandcode.WireContentPart{{
					Type:       "tool-result",
					ToolCallID: m.ToolCallID,
					ToolName:   m.Name,
					Output:     &commandcode.WireOutput{Type: "text", Value: m.ContentText()},
				}},
			})

		default:
			return "", nil, fmt.Errorf("convert: unsupported role %q", m.Role)
		}
	}
	return strings.Join(sysParts, "\n"), out, nil
}

// convertToolChoice maps OpenAI tool_choice onto the Anthropic-style wire form:
// "auto"→auto, "none"→none, "required"→any, {"type":"function"}→{"type":"tool",name}.
func convertToolChoice(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return commandcode.WireToolChoice{Type: "auto"}, nil
		case "none":
			return commandcode.WireToolChoice{Type: "none"}, nil
		case "required":
			return commandcode.WireToolChoice{Type: "any"}, nil
		default:
			return nil, fmt.Errorf("convert: unknown tool_choice %q", s)
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Type == "function" {
		return commandcode.WireToolChoice{Type: "tool", Name: obj.Function.Name}, nil
	}
	return nil, fmt.Errorf("convert: unsupported tool_choice shape")
}

// parseArguments parses an OpenAI tool-call arguments JSON string.
// Invalid JSON degrades to an empty object (upstream expects an object).
func parseArguments(args string) map[string]any {
	out := map[string]any{}
	if args != "" {
		_ = json.Unmarshal([]byte(args), &out)
	}
	return out
}

// mimeFromDataURL extracts the MIME type from a data URL, defaulting to png.
func mimeFromDataURL(u string) string {
	if strings.HasPrefix(u, "data:") {
		if i := strings.Index(u[5:], ";"); i > 0 {
			return u[5 : 5+i]
		}
		if i := strings.Index(u[5:], ","); i > 0 {
			return u[5 : 5+i]
		}
	}
	return "image/png"
}
