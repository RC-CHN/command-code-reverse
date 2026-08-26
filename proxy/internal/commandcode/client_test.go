package commandcode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestGenerateHeadersAndBody(t *testing.T) {
	var gotHeaders http.Header
	var gotBody GenerateRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = w.Write([]byte("{\"type\":\"start\"}\n{\"type\":\"finish\"}\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, func() string { return "1.32.2" }, srv.Client())
	rc, err := c.Generate(context.Background(), Credentials{
		APIKey:      "k1",
		SessionID:   "sess-1",
		ProjectSlug: "d-users-dev-projects-x-a3f2",
	}, &GenerateRequest{Mode: "agent", PermissionMode: "standard"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	defer func() { _ = rc.Close() }()

	body, _ := io.ReadAll(rc)
	if !strings.Contains(string(body), `"type":"start"`) {
		t.Errorf("body = %q", body)
	}

	for h, want := range map[string]string{
		"Authorization":          "Bearer k1",
		"User-Agent":             "cli",
		"X-Command-Code-Version": "1.32.2",
		"X-Cli-Environment":      "production",
		"X-Taste-Learning":       "false",
		"X-Session-Id":           "sess-1",
		"X-Project-Slug":         "d-users-dev-projects-x-a3f2",
	} {
		if got := gotHeaders.Get(h); got != want {
			t.Errorf("header %s = %q, want %q", h, got, want)
		}
	}
	if tp := gotHeaders.Get("Traceparent"); !strings.HasPrefix(tp, "00-") || !strings.HasSuffix(tp, "-01") {
		t.Errorf("traceparent = %q", tp)
	}
	// The old proxy's phantom header must NOT be present.
	if got := gotHeaders.Get("X-Co-Flag"); got != "" {
		t.Errorf("phantom x-co-flag present: %q", got)
	}
	if gotBody.Mode != "agent" || gotBody.PermissionMode != "standard" {
		t.Errorf("request body = %+v", gotBody)
	}
}

func TestGenerateAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"error":{"code":"BAD_REQUEST","status":400,"message":"insufficient credits. Buy more.","docs":"https://x"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, func() string { return "1.32.2" }, srv.Client())
	_, err := c.Generate(context.Background(), Credentials{APIKey: "k1"}, &GenerateRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err type = %T", err)
	}
	if ae.Status != 400 || ae.Code != "BAD_REQUEST" {
		t.Errorf("ae = %+v", ae)
	}
	if !ae.IsInsufficientCredits() {
		t.Error("IsInsufficientCredits should be true")
	}
	if ae.IsRateLimited() {
		t.Error("IsRateLimited should be false")
	}
}

func TestTraceparentRetrySemantics(t *testing.T) {
	c := NewClient("http://x", func() string { return "test" }, nil)
	creds := Credentials{APIKey: "k", SessionID: "s", ProjectSlug: "p", TraceID: "0123456789abcdef0123456789abcdef"}

	re := regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-01$`)
	m1 := re.FindStringSubmatch(c.headers(creds)["traceparent"])
	m2 := re.FindStringSubmatch(c.headers(creds)["traceparent"])
	if m1 == nil || m2 == nil {
		t.Fatalf("traceparent shape wrong: %q", c.headers(creds)["traceparent"])
	}
	if m1[1] != creds.TraceID || m2[1] != creds.TraceID {
		t.Error("retry attempts must share the pinned trace ID")
	}
	if m1[2] == m2[2] {
		t.Error("each attempt must re-roll the span ID")
	}

	// No pinned trace ID → fully random per call (non-retry paths).
	creds.TraceID = ""
	a := c.headers(creds)["traceparent"]
	b := c.headers(creds)["traceparent"]
	if a == b {
		t.Error("unpinned traceparent must be random per call")
	}
}

func TestProjectSlug(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\dev\projects\app-a3f2`: "c-users-dev-projects-app-a3f2",
		"/home/dev/My App!":              "home-dev-my-app",
		"---weird---":                    "weird",
	} {
		if got := ProjectSlug(in); got != want {
			t.Errorf("ProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
