package commandcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	JevDefaultChoiceOptions = 20
	JevMaxChoiceOptions     = 255
	JevMaxQuestions         = 20
)

// SystemOneRequest keeps the wire JSON intact, including large integers and
// extension fields. Questions are decoded separately for validation.
type SystemOneRequest struct {
	Model     string
	Body      json.RawMessage
	Questions map[string]SystemOneQuestion
}

type SystemOneQuestion struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

type SystemOneResponse struct {
	Body  json.RawMessage
	Usage *SystemOneUsage
}

type SystemOneUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// RequestError is a local rejection; it must never reach the key pool.
type RequestError struct {
	Status  int
	Code    string
	Param   string
	Message string
}

func (e *RequestError) Error() string { return e.Message }

func invalidSystemOne(param, message string) error {
	return &RequestError{Status: 422, Code: "invalid_request_error", Param: param, Message: message}
}

func ResolveSystemOneModel(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "jev", "jev-latest", "typesafe/jev", "typesafe-ai/jev":
		return "typesafe/jev"
	default:
		return ""
	}
}

// ParseSystemOneRequest applies the CLI's question schema plus the proxy's
// explicit opt-in for more than 20 Choice options (upstream maximum: 255).
func ParseSystemOneRequest(raw []byte, unlock bool) (*SystemOneRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, &RequestError{Status: 400, Code: "invalid_json", Message: "Request body must be a single JSON object"}
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return nil, invalidSystemOne("model", "model is required and must be a string")
	}
	model = ResolveSystemOneModel(model)
	if model == "" {
		return nil, &RequestError{Status: 400, Code: "unsupported_model", Param: "model", Message: "This endpoint supports typesafe/jev (aliases: jev, jev-latest, typesafe-ai/jev)"}
	}
	if _, ok := fields["stream"]; ok {
		return nil, invalidSystemOne("stream", "System One returns one JSON response; omit stream")
	}
	if !systemOneValue(fields["state"]) {
		return nil, invalidSystemOne("state", "state is required and must be a string, object, array, or null")
	}
	var questions map[string]SystemOneQuestion
	if json.Unmarshal(fields["questions"], &questions) != nil || len(questions) == 0 || len(questions) > JevMaxQuestions {
		return nil, invalidSystemOne("questions", "questions must be an object containing 1 to 20 named questions")
	}
	limit := JevDefaultChoiceOptions
	if unlock {
		limit = JevMaxChoiceOptions
	}
	for name, question := range questions {
		param := "questions." + name
		if name == "" {
			return nil, invalidSystemOne("questions", "Question names must not be empty")
		}
		if !systemOneValue(question.Instructions) {
			return nil, invalidSystemOne(param+".instructions", "instructions is required and must be a string, object, array, or null")
		}
		if err := validateSystemOneCriteria(question, param+".criteria", limit); err != nil {
			return nil, err
		}
	}
	fields["model"], _ = json.Marshal(model)
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return &SystemOneRequest{Model: model, Body: body, Questions: questions}, nil
}

func validateSystemOneCriteria(q SystemOneQuestion, param string, limit int) error {
	switch q.Type {
	case "noul", "choice":
		if q.Type == "noul" && (len(q.Criteria) == 0 || bytes.Equal(bytes.TrimSpace(q.Criteria), []byte("null"))) {
			return nil
		}
		var criteria map[string]json.RawMessage
		if json.Unmarshal(q.Criteria, &criteria) != nil || criteria == nil {
			return invalidSystemOne(param, "criteria must be an object")
		}
		if q.Type == "choice" {
			if len(criteria) == 0 {
				return invalidSystemOne(param, "Choice requires at least one option")
			}
			if len(criteria) > JevMaxChoiceOptions {
				return &RequestError{Status: 422, Code: "jev_choice_options_exceeded", Param: param, Message: "Choice supports at most 255 options, even when unlocked"}
			}
			if len(criteria) > limit {
				return &RequestError{Status: 422, Code: "jev_choice_options_locked", Param: param,
					Message: fmt.Sprintf("Choice has %d options; the proxy limit is %d. Set JEV_UNLOCK_MAX_OPTIONS=true and restart the proxy to allow up to 255", len(criteria), limit)}
			}
		}
		for name, value := range criteria {
			if q.Type == "noul" && name != "true" && name != "false" {
				return invalidSystemOne(param, "Noul criteria may only contain true and false")
			}
			if !systemOneValue(value) {
				return invalidSystemOne(param, "Criterion values must be strings, objects, arrays, or null")
			}
		}
	case "score":
		var levels []json.RawMessage
		if json.Unmarshal(q.Criteria, &levels) != nil || len(levels) < 2 || len(levels) > 10 {
			return invalidSystemOne(param, "Score criteria must contain 2 to 10 ordered levels")
		}
		for _, value := range levels {
			if !systemOneValue(value) {
				return invalidSystemOne(param, "Score levels must be strings, objects, arrays, or null")
			}
		}
	default:
		return invalidSystemOne(strings.TrimSuffix(param, "criteria")+"type", "Question type must be noul, choice, or score")
	}
	return nil
}

// The CLI permits null as well as structured/text input; nested JSON values
// are left unmodified. All inputs have already passed json.Unmarshal.
func systemOneValue(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && (raw[0] == '"' || raw[0] == '{' || raw[0] == '[' || bytes.Equal(raw, []byte("null")))
}

// SystemOne waits for the entire JSON result before declaring success. A
// truncated/invalid response can never be returned as HTTP 200 downstream.
func (c *Client) SystemOne(ctx context.Context, creds Credentials, wire *SystemOneRequest, maxResponseBytes int64) (*SystemOneResponse, error) {
	if maxResponseBytes <= 0 || maxResponseBytes == 1<<63-1 {
		return nil, fmt.Errorf("invalid System One response limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/provider/v1/systemone", bytes.NewReader(wire.Body))
	if err != nil {
		return nil, err
	}
	for name, value := range c.headers(creds) {
		req.Header.Set(name, value)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("systemone request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := parseAPIError(resp)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read systemone response: %w", err)
	}
	if int64(len(raw)) > maxResponseBytes {
		return nil, &APIError{Status: 502, Code: "upstream_response_too_large", Message: "System One response exceeds JEV_MAX_RESPONSE_BYTES"}
	}
	return decodeSystemOneResponse(raw, wire)
}

func decodeSystemOneResponse(raw []byte, req *SystemOneRequest) (*SystemOneResponse, error) {
	invalid := &APIError{Status: 502, Code: "upstream_invalid_response", Message: "System One returned an invalid or incomplete JSON response"}
	var response struct {
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   *SystemOneUsage            `json:"usage"`
		Error   json.RawMessage            `json:"error"`
		Success *bool                      `json:"success"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Answers) != len(req.Questions) || response.Answers == nil ||
		(len(response.Error) > 0 && string(response.Error) != "null") || (response.Success != nil && !*response.Success) {
		return nil, invalid
	}
	if response.Usage != nil && (response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0) {
		return nil, invalid
	}
	for name, question := range req.Questions {
		var answer struct {
			Type          string                     `json:"type"`
			Noul          *float64                   `json:"noul"`
			Choice        *string                    `json:"choice"`
			Score         *float64                   `json:"score"`
			Confidence    *float64                   `json:"confidence"`
			Probabilities map[string]float64         `json:"probabilities"`
			Legend        map[string]json.RawMessage `json:"legend"`
		}
		if json.Unmarshal(response.Answers[name], &answer) != nil || answer.Type != question.Type {
			return nil, invalid
		}
		if answer.Confidence != nil && (*answer.Confidence < 0 || *answer.Confidence > 1) {
			return nil, invalid
		}
		for _, value := range answer.Probabilities {
			if value < 0 || value > 1 {
				return nil, invalid
			}
		}
		for _, value := range answer.Legend {
			if !systemOneValue(value) {
				return nil, invalid
			}
		}
		switch question.Type {
		case "noul":
			if answer.Noul == nil || *answer.Noul < 0 || *answer.Noul > 1 {
				return nil, invalid
			}
		case "choice":
			var criteria map[string]json.RawMessage
			if json.Unmarshal(question.Criteria, &criteria) != nil || answer.Choice == nil {
				return nil, invalid
			}
			if _, ok := criteria[*answer.Choice]; !ok {
				return nil, invalid
			}
			if answer.Probabilities != nil {
				if len(answer.Probabilities) != len(criteria) {
					return nil, invalid
				}
				for option := range answer.Probabilities {
					if _, ok := criteria[option]; !ok {
						return nil, invalid
					}
				}
			}
		case "score":
			var levels []json.RawMessage
			if json.Unmarshal(question.Criteria, &levels) != nil || answer.Score == nil || *answer.Score < 0 || *answer.Score > float64(len(levels)-1) {
				return nil, invalid
			}
		}
	}
	return &SystemOneResponse{Body: raw, Usage: response.Usage}, nil
}
