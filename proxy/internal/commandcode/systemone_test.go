package commandcode

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const systemOneNoul = `{"model":"jev-latest","state":{"id":9007199254740993},"questions":{"q":{"type":"noul","instructions":"Is it valid?"}},"extension":{"id":9007199254740993}}`
const systemOneAnswer = `{"model":"typesafe/jev","answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":5,"output_tokens":2},"extension":9007199254740993}`

func TestSystemOneValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"noul", systemOneNoul, true},
		{"structured", `{"model":"jev","state":null,"questions":{"q":{"type":"noul","instructions":{"nested":[true,2]},"criteria":{"true":null,"false":[]}}}}`, true},
		{"score", `{"model":"typesafe-ai/jev","state":"s","questions":{"q":{"type":"score","instructions":[],"criteria":["low","high"]}}}`, true},
		{"choice", `{"model":"jev","state":[],"questions":{"q":{"type":"choice","instructions":"pick","criteria":{"a":null,"b":{"text":"B"}}}}}`, true},
		{"missing state", `{"model":"jev","questions":{"q":{"type":"noul","instructions":"?"}}}`, false},
		{"missing instructions", `{"model":"jev","state":{},"questions":{"q":{"type":"noul"}}}`, false},
		{"scalar state", strings.Replace(systemOneNoul, `{"id":9007199254740993}`, "1", 1), false},
		{"missing model", `{"state":{},"questions":{}}`, false},
		{"unknown model", strings.Replace(systemOneNoul, "jev-latest", "other", 1), false},
		{"stream", strings.Replace(systemOneNoul, `"extension":`, `"stream":false,"extension":`, 1), false},
		{"empty questions", `{"model":"jev","state":{},"questions":{}}`, false},
		{"empty name", `{"model":"jev","state":{},"questions":{"":{"type":"noul","instructions":"?"}}}`, false},
		{"null question", `{"model":"jev","state":{},"questions":{"q":null}}`, false},
		{"unknown type", strings.Replace(systemOneNoul, `"noul"`, `"unknown"`, 1), false},
		{"score few", `{"model":"jev","state":{},"questions":{"q":{"type":"score","instructions":"?","criteria":["one"]}}}`, false},
		{"score many", `{"model":"jev","state":{},"questions":{"q":{"type":"score","instructions":"?","criteria":[0,1,2,3,4,5,6,7,8,9,10]}}}`, false},
		{"choice empty", `{"model":"jev","state":{},"questions":{"q":{"type":"choice","instructions":"?","criteria":{}}}}`, false},
		{"criterion scalar", `{"model":"jev","state":{},"questions":{"q":{"type":"choice","instructions":"?","criteria":{"a":1}}}}`, false},
		{"trailing JSON", systemOneNoul + "{}", false},
		{"null body", "null", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseSystemOneRequest([]byte(tc.body), false)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err == nil && req.Model != "typesafe/jev" {
				t.Fatal("model alias not normalized")
			}
		})
	}
}

func TestSystemOneQuestionCount(t *testing.T) {
	for _, count := range []int{20, 21} {
		questions := map[string]any{}
		for i := range count {
			questions[strings.Repeat("q", i+1)] = map[string]string{"type": "noul", "instructions": "?"}
		}
		body, _ := json.Marshal(map[string]any{"model": "jev", "state": nil, "questions": questions})
		_, err := ParseSystemOneRequest(body, true)
		if (err == nil) != (count == 20) {
			t.Fatalf("count=%d err=%v", count, err)
		}
	}
}

func TestSystemOneClientResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		limit            int64
		truncated        bool
	}{
		{"success", systemOneAnswer, "", 1 << 20, false},
		{"missing answers", `{}`, "upstream_invalid_response", 1 << 20, false},
		{"error with 200", `{"error":{"code":"bad"}}`, "upstream_invalid_response", 1 << 20, false},
		{"missing value", `{"answers":{"q":{"type":"noul"}}}`, "upstream_invalid_response", 1 << 20, false},
		{"wrong type", `{"answers":{"q":{"type":"score","score":0.5}}}`, "upstream_invalid_response", 1 << 20, false},
		{"missing question", `{"answers":{"other":{"type":"noul","noul":0.5}}}`, "upstream_invalid_response", 1 << 20, false},
		{"out of range", `{"answers":{"q":{"type":"noul","noul":1.5}}}`, "upstream_invalid_response", 1 << 20, false},
		{"negative usage", `{"answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":-1}}`, "upstream_invalid_response", 1 << 20, false},
		{"oversize", systemOneAnswer, "upstream_response_too_large", 16, false},
		{"malformed", `{"answers":`, "upstream_invalid_response", 1 << 20, false},
		{"trailing", systemOneAnswer + "{}", "upstream_invalid_response", 1 << 20, false},
		{"truncated", systemOneAnswer, "transport", 1 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.truncated {
					w.Header().Set("Content-Length", "10000")
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client := NewClient(srv.URL, func() string { return "test" }, true, srv.Client())
			req, err := ParseSystemOneRequest([]byte(systemOneNoul), false)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.SystemOne(t.Context(), Credentials{APIKey: "synthetic"}, req, tc.limit)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				if string(response.Body) != tc.body || response.Usage.InputTokens != 5 {
					t.Fatal("response changed")
				}
				return
			}
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			if tc.code != "transport" {
				var ae *APIError
				if !errors.As(err, &ae) || ae.Code != tc.code {
					t.Fatalf("err=%v", err)
				}
			}
		})
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"45", 45 * time.Second}, {now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{"-1", 0}, {"0", 0}, {"9223372036854775807", 0}, {"not a date", 0},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
	} {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Fatalf("%s: %v", tc.value, got)
		}
	}
}
