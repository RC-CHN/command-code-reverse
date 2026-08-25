// Package commandcode defines the upstream wire types for the Command Code
// API and a minimal HTTP client for POST /alpha/generate.
//
// Protocol reference (reverse engineered from command-code@1.32.2):
//   - Request:  POST {base}/alpha/generate, JSON body, NDJSON stream response.
//   - Response: newline-delimited JSON events (NOT SSE).
package commandcode

// GenerateRequest is the top-level body for POST /alpha/generate.
type GenerateRequest struct {
	Config         RequestConfig `json:"config"`
	Memory         any           `json:"memory"` // always null
	Taste          any           `json:"taste"`  // always null
	Skills         any           `json:"skills"` // always null
	PermissionMode string        `json:"permissionMode"`
	Mode           string        `json:"mode"`               // "agent" for chat
	ThreadID       string        `json:"threadId,omitempty"` // only when a valid UUID
	Params         Params        `json:"params"`
}

// RequestConfig carries workspace context. The proxy sends a stable,
// mostly-empty shape to stay cache-friendly.
type RequestConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"` // YYYY-MM-DD
	Environment   string   `json:"environment"`
	Structure     []string `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

// Params holds the actual inference parameters.
type Params struct {
	Model           string        `json:"model"`
	Messages        []WireMessage `json:"messages"`
	Tools           []WireTool    `json:"tools,omitempty"`
	System          string        `json:"system,omitempty"`
	MaxTokens       int           `json:"max_tokens"`
	Stream          bool          `json:"stream"` // always true upstream
	Temperature     *float64      `json:"temperature,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	ToolChoice      any           `json:"tool_choice,omitempty"`
}

// WireMessage is a message in upstream format. Role is "user" | "assistant" | "tool".
type WireMessage struct {
	Role    string            `json:"role"`
	Content []WireContentPart `json:"content"`
}

// WireContentPart is a single content part. Exactly one field group is
// populated per part, discriminated by Type:
//   - "text":        Text
//   - "image":       Image (data URL) + MimeType
//   - "tool-call":   ToolCallID + ToolName + Input          (assistant)
//   - "tool-result": ToolCallID + ToolName + Output          (tool role)
//   - "reasoning":   Text                                    (assistant)
type WireContentPart struct {
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	Image      string         `json:"image,omitempty"`
	MimeType   string         `json:"mimeType,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	ToolName   string         `json:"toolName,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
	Output     *WireOutput    `json:"output,omitempty"`
}

// WireOutput wraps a tool result value.
type WireOutput struct {
	Type  string `json:"type"` // "text"
	Value string `json:"value"`
}

// WireTool is a tool definition in upstream format.
type WireTool struct {
	Type        string `json:"type"` // "function"
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

// WireToolChoice object form: {"type":"auto"|"any"|"none"} or {"type":"tool","name":...}.
type WireToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}
