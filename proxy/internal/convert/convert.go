// Package convert translates OpenAI chat completion requests into the
// Command Code wire format (and back, for responses). All functions are
// pure and table-tested.
package convert

import (
	"crypto/sha256"
	"encoding/hex"
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
	PromptCache     string          `json:"prompt_cache,omitempty"` // extension: "off"
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
	Type         string                    `json:"type"`
	Text         string                    `json:"text,omitempty"`
	CacheControl *commandcode.CacheControl `json:"cache_control,omitempty"`
	ImageURL     *struct {
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
	if req.PromptCache != "" && req.PromptCache != "off" {
		return nil, fmt.Errorf("convert: prompt_cache must be empty or off")
	}

	system, messages, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	wireSystem, err := convertSystem(req.Messages, system)
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
		PromptCache:    req.PromptCache,
		Params: commandcode.Params{
			Model:           req.Model,
			Messages:        messages,
			System:          wireSystem,
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

// convertSystem uses the string wire form unless the caller explicitly
// marks a system/developer text block for caching. Text remains identical
// to convertMessages, including separators between system messages.
func convertSystem(msgs []Message, fallback string) (any, error) {
	var sections []commandcode.WireSystemPart
	marked := false
	for _, m := range msgs {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		var parts []commandcode.WireSystemPart
		if raw := m.ContentParts(); raw != nil {
			for _, p := range raw {
				if p.CacheControl != nil && (p.Type != "text" || p.CacheControl.Type != "ephemeral") {
					return nil, fmt.Errorf("convert: system cache_control requires a text block and type ephemeral")
				}
				if p.Type == "text" {
					parts = append(parts, commandcode.WireSystemPart{Type: "text", Text: p.Text, CacheControl: p.CacheControl})
					marked = marked || p.CacheControl != nil
				}
			}
		} else if text := m.ContentText(); text != "" {
			parts = append(parts, commandcode.WireSystemPart{Type: "text", Text: text})
		}
		if m.ContentText() == "" {
			continue
		}
		if len(sections) > 0 {
			sections[len(sections)-1].Text += "\n"
		}
		sections = append(sections, parts...)
	}
	if marked && len(sections) > 0 {
		return sections, nil
	}
	if fallback == "" {
		return nil, nil
	}
	return fallback, nil
}

// ConversationRoot returns a stable fingerprint of a conversation:
// leading system/developer context plus the first non-system message.
// System prompts differ wildly across clients, making the root unique per
// (client, conversation) pair; prefix-chain growth keeps both parts
// constant from the very first request. Only LEADING system messages count
// — mid-conversation system injections must not move the root. Clients
// without a system prompt hash an empty system section, which is fine.
func ConversationRoot(msgs []Message) string {
	h := sha256.New()
	for _, m := range msgs {
		writeContent := func() {
			if t := m.ContentText(); t != "" {
				h.Write([]byte(t))
			} else if len(m.Content) > 0 {
				h.Write(m.Content)
			}
			h.Write([]byte{0})
		}
		if m.Role == "system" || m.Role == "developer" {
			h.Write([]byte("system"))
			h.Write([]byte{0})
			writeContent()
			continue
		}
		// First turn message completes the root.
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		writeContent()
		break
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// convertMessages splits OpenAI messages into a system prompt plus wire
// messages. System/developer messages are joined with newlines.
func convertMessages(msgs []Message) (string, []commandcode.WireMessage, error) {
	var sysParts []string
	var out []commandcode.WireMessage
	// toolCallID → toolName, collected from assistant tool_calls. OpenAI
	// marks "name" on tool messages as optional, but the upstream schema
	// requires a non-empty toolName on every tool-result part. Resolution
	// order mirrors the CLI: explicit name → map lookup → "unknown".
	toolNames := map[string]string{}

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
							if !strings.HasPrefix(p.ImageURL.URL, "data:") {
								// The upstream wire format only carries base64
								// data URLs (CLI parseDataUrl); a remote URL
								// would be silently ignored by the model.
								return "", nil, fmt.Errorf(
									"image_url must be a base64 data URL (data:<mime>;base64,...), "+
										"remote URLs are not supported: %.80s", p.ImageURL.URL)
							}
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
				toolNames[tc.ID] = tc.Function.Name
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
			name := m.Name
			if name == "" {
				name = toolNames[m.ToolCallID]
			}
			if name == "" {
				name = "unknown"
			}
			out = append(out, commandcode.WireMessage{
				Role: "tool",
				Content: []commandcode.WireContentPart{{
					Type:       "tool-result",
					ToolCallID: m.ToolCallID,
					ToolName:   name,
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
