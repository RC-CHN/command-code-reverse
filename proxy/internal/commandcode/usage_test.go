package commandcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUsageFollowsCLIOrganizationAndBillingPeriod(t *testing.T) {
	var reports atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer billing-key" || r.Header.Get("X-Cmd-Zdr") != "1" {
			t.Error("billing authentication or ZDR changed")
		}
		if r.URL.Path != "/alpha/whoami" && r.URL.Query().Get("orgId") != "org+space &" {
			t.Error("organization scope lost")
		}
		switch r.URL.Path {
		case "/alpha/whoami":
			if r.URL.Query().Get("limits") != "1" {
				t.Error("missing limits query")
			}
			_, _ = w.Write([]byte(`{"user":{"email":"private-email"},"org":{"id":"org+space &"},"orgLimits":[{"scope":"model","model":"test","limit":12,"spent":2}]}`))
		case "/alpha/billing/credits":
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":9,"purchasedCredits":0,"freeCredits":1},"windowLimits":{"limited":true,"fiveHour":{"used":1,"cap":5,"resetAt":1791860000000}}}`))
		case "/alpha/billing/subscriptions":
			_, _ = w.Write([]byte(`{"data":{"id":"private-stripe-id","planId":"plan","currentPeriodStart":"2026-10-01T00:00:00Z","currentPeriodEnd":"2026-11-01T00:00:00Z"}}`))
		case "/alpha/usage/summary":
			if r.URL.Query().Get("since") != "2026-10-01T00:00:00Z" {
				t.Error("billing-period start lost")
			}
			_, _ = w.Write([]byte(`{"totalCount":5,"totalCost":0,"totalTokensIn":20,"totalTokensOut":10,"periodBasis":"billing-period"}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, func() string { return "test" }, true, srv.Client())
	c.SetKeyObserver(func(string, string) func() { reports.Add(1); return nil })
	u, err := c.Usage(context.Background(), "billing-key")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Errors) != 0 || *u.Credits.Monthly != 9 || *u.Summary.TotalCount != 5 || u.Summary.TotalCost == nil || *u.Summary.TotalCost != 0 {
		t.Fatalf("usage: %+v", u)
	}
	raw, _ := json.Marshal(u)
	if strings.Contains(string(raw), "private-") || strings.Contains(string(raw), "org+space") || reports.Load() != 0 {
		t.Fatal("identity leak or billing activated telemetry")
	}
}

func TestUsagePartialFailuresDoNotLeakErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alpha/billing/credits" {
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":0,"purchasedCredits":0,"freeCredits":0}}`))
			return
		}
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"message":"private-account-secret"}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, func() string { return "test" }, false, srv.Client())
	u, err := c.Usage(context.Background(), "key")
	if err != nil || u.Credits == nil || u.Summary != nil || len(u.Errors) != 3 {
		t.Fatalf("partial billing: %+v %v", u, err)
	}
	raw, _ := json.Marshal(u)
	if strings.Contains(string(raw), "private-account-secret") {
		t.Fatal("raw upstream error exposed")
	}
}
