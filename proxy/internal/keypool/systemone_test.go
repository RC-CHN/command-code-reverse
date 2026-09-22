package keypool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

type systemOneFunc func(context.Context, commandcode.Credentials, *commandcode.SystemOneRequest, int64) (*commandcode.SystemOneResponse, error)

func (f systemOneFunc) SystemOne(ctx context.Context, c commandcode.Credentials, r *commandcode.SystemOneRequest, maxBytes int64) (*commandcode.SystemOneResponse, error) {
	return f(ctx, c, r, maxBytes)
}

func TestSystemOneRotationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name           string
		err            error
		rotate, broken bool
	}{
		{"401", &commandcode.APIError{Status: 401}, true, true},
		{"auth 403", &commandcode.APIError{Status: 403, Type: "authentication_error"}, true, true},
		{"402", &commandcode.APIError{Status: 402}, true, true},
		{"credits", &commandcode.APIError{Status: 400, Message: "insufficient credits"}, true, true},
		{"429", &commandcode.APIError{Status: 429}, true, true},
		{"upgrade", &commandcode.APIError{Status: 403, Code: "upgrade_required"}, true, false},
		{"model plan", &commandcode.APIError{Status: 403, Code: "MODEL_NOT_IN_PLAN"}, true, false},
		{"unknown 403", &commandcode.APIError{Status: 403, Type: "permission_error"}, false, false},
		{"unsupported model", &commandcode.APIError{Status: 400, Code: "unsupported_model"}, false, false},
		{"bad request", &commandcode.APIError{Status: 400, Code: "invalid_request_error"}, false, false},
		{"validation", &commandcode.APIError{Status: 422}, false, false},
		{"spend cap", &commandcode.APIError{Status: 429, Code: "USAGE_EXCEEDED"}, false, false},
		{"ZDR", &commandcode.APIError{Status: 422, Type: "cmd_zdr_no_providers"}, false, false},
		{"503", &commandcode.APIError{Status: 503}, false, false},
		{"529", &commandcode.APIError{Status: 529}, false, false},
		{"network", errors.New("connection reset"), false, false},
		{"timeout", context.DeadlineExceeded, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []commandcode.Credentials
			fail := true
			client := systemOneFunc(func(_ context.Context, creds commandcode.Credentials, _ *commandcode.SystemOneRequest, _ int64) (*commandcode.SystemOneResponse, error) {
				calls = append(calls, creds)
				if fail && creds.APIKey == "k1" {
					return nil, tc.err
				}
				return &commandcode.SystemOneResponse{}, nil
			})
			pool := NewSystemOne(client, session.NewStore("seed"), []string{"k1", "k2"}, BreakerPolicy{}, 1024)
			meta := commandcode.CallMeta{Root: "r", TraceID: "0123456789abcdef0123456789abcdef"}
			_, err := pool.SystemOne(t.Context(), "", meta, &commandcode.SystemOneRequest{})
			wantCalls := 1
			if tc.rotate {
				wantCalls = 2
			}
			if len(calls) != wantCalls || (err == nil) != tc.rotate {
				t.Fatalf("calls=%d err=%v", len(calls), err)
			}
			if !tc.rotate && !errors.Is(err, tc.err) {
				t.Fatal("original error lost")
			}
			if tc.rotate && (calls[0].SessionID == calls[1].SessionID || calls[0].TraceID != calls[1].TraceID) {
				t.Fatal("rotation changed trace or reused account session")
			}
			if pool.pool.Snapshot()[0]["broken"] != tc.broken {
				t.Fatal("wrong breaker state")
			}
			fail = false
			_, err = pool.SystemOne(t.Context(), "", meta, &commandcode.SystemOneRequest{})
			if err != nil {
				t.Fatal(err)
			}
			wantKey := "k1"
			if tc.broken {
				wantKey = "k2"
			}
			if calls[len(calls)-1].APIKey != wantKey {
				t.Fatal("incorrect next key")
			}
		})
	}
}

func TestSystemOneAndChatBreakersAreIndependent(t *testing.T) {
	for _, breakChat := range []bool{false, true} {
		fc := &fakeClient{}
		chat := New(fc, session.NewStore("seed"), []string{"k1"}, BreakerPolicy{})
		var jevErr error
		jev := NewSystemOne(systemOneFunc(func(context.Context, commandcode.Credentials, *commandcode.SystemOneRequest, int64) (*commandcode.SystemOneResponse, error) {
			return &commandcode.SystemOneResponse{}, jevErr
		}), session.NewStore("seed"), []string{"k1"}, BreakerPolicy{}, 1024)
		if breakChat {
			fc.failWith = map[string]error{"k1": &commandcode.APIError{Status: 401}}
		} else {
			jevErr = &commandcode.APIError{Status: 429}
		}
		body, chatErr := chat.Generate(t.Context(), "", commandcode.CallMeta{}, &commandcode.GenerateRequest{})
		if body != nil {
			_ = body.Close()
		}
		_, systemErr := jev.SystemOne(t.Context(), "", commandcode.CallMeta{}, &commandcode.SystemOneRequest{})
		if (chatErr != nil) != breakChat || (systemErr != nil) == breakChat {
			t.Fatalf("chat=%v jev=%v", chatErr, systemErr)
		}
		fc.failWith = nil
		jevErr = nil
		// Success in the other API must not clear the original circuit.
		body, chatErr = chat.Generate(t.Context(), "", commandcode.CallMeta{}, &commandcode.GenerateRequest{})
		if body != nil {
			_ = body.Close()
		}
		_, systemErr = jev.SystemOne(t.Context(), "", commandcode.CallMeta{}, &commandcode.SystemOneRequest{})
		var unavailable *UnavailableError
		if breakChat {
			if !errors.As(chatErr, &unavailable) || systemErr != nil {
				t.Fatalf("chat=%v jev=%v", chatErr, systemErr)
			}
		} else if chatErr != nil || !errors.As(systemErr, &unavailable) {
			t.Fatalf("chat=%v jev=%v", chatErr, systemErr)
		}
	}
}

func TestSystemOneRetryAfterAndPassthrough(t *testing.T) {
	now := time.Now()
	var calls []string
	fail := true
	client := systemOneFunc(func(_ context.Context, c commandcode.Credentials, _ *commandcode.SystemOneRequest, _ int64) (*commandcode.SystemOneResponse, error) {
		calls = append(calls, c.APIKey)
		if fail {
			return nil, &commandcode.APIError{Status: 429, RetryAfter: 45 * time.Second}
		}
		return &commandcode.SystemOneResponse{}, nil
	})
	p := NewSystemOne(client, session.NewStore("seed"), []string{"k1"}, BreakerPolicy{Now: func() time.Time { return now }}, 1024)
	_, _ = p.SystemOne(t.Context(), "caller", commandcode.CallMeta{}, &commandcode.SystemOneRequest{})
	if len(calls) != 1 || calls[0] != "caller" || p.pool.Snapshot()[0]["broken"].(bool) {
		t.Fatal("passthrough touched pool")
	}
	_, _ = p.SystemOne(t.Context(), "", commandcode.CallMeta{}, &commandcode.SystemOneRequest{})
	now = now.Add(5 * time.Second)
	_, err := p.SystemOne(t.Context(), "", commandcode.CallMeta{}, &commandcode.SystemOneRequest{})
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || unavailable.RetryAfter != 40*time.Second || len(calls) != 2 {
		t.Fatalf("err=%v calls=%v", err, calls)
	}
	now = now.Add(40 * time.Second)
	fail = false
	if _, err = p.SystemOne(t.Context(), "", commandcode.CallMeta{}, &commandcode.SystemOneRequest{}); err != nil {
		t.Fatal(err)
	}
}
