package commandcode

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AccountUsage is an allowlist for the administration surface. Whoami identity,
// Stripe IDs and subscription metadata never leave the proxy.
type AccountUsage struct {
	FetchedAt    time.Time      `json:"fetchedAt"`
	Credits      *CreditBalance `json:"credits"`
	Subscription *Subscription  `json:"subscription"`
	Summary      *UsageSummary  `json:"summary"`
	WindowLimits *WindowLimits  `json:"windowLimits"`
	OrgLimits    []OrgLimit     `json:"orgLimits"`
	Errors       []UsageError   `json:"errors"`
}

type CreditBalance struct {
	Monthly   *float64 `json:"monthlyCredits"`
	Purchased *float64 `json:"purchasedCredits"`
	Free      *float64 `json:"freeCredits"`
}

type Subscription struct {
	PlanID             string `json:"planId"`
	Status             string `json:"status"`
	CurrentPeriodStart string `json:"currentPeriodStart"`
	CurrentPeriodEnd   string `json:"currentPeriodEnd"`
	CancelAtPeriodEnd  bool   `json:"cancelAtPeriodEnd"`
}

type UsageSummary struct {
	TotalCount     *int64   `json:"totalCount"`
	TotalCost      *float64 `json:"totalCost"`
	CompletedCount *int64   `json:"completedCount"`
	FailedCount    *int64   `json:"failedCount"`
	TotalTokensIn  *int64   `json:"totalTokensIn"`
	TotalTokensOut *int64   `json:"totalTokensOut"`
	PeriodBasis    string   `json:"periodBasis"`
}

type UsageWindow struct {
	Used     *float64 `json:"used"`
	Cap      *float64 `json:"cap"`
	ResetAt  int64    `json:"resetAt"`
	Exceeded bool     `json:"exceeded"`
}

type WindowLimits struct {
	Limited  bool         `json:"limited"`
	FiveHour *UsageWindow `json:"fiveHour"`
	Weekly   *UsageWindow `json:"weekly"`
}

type OrgLimit struct {
	Scope         string          `json:"scope"`
	Model         string          `json:"model"`
	ModelLabel    string          `json:"modelLabel"`
	Limit         *float64        `json:"limit"`
	Spent         *float64        `json:"spent"`
	Exceeded      bool            `json:"exceeded"`
	ResetInterval string          `json:"resetInterval"`
	ResetAt       json.RawMessage `json:"resetAt"`
}

type UsageError struct {
	Section string `json:"section"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message"`
}

// Usage follows the CLI's fetchUsageData: whoami?limits=1, organization-aware
// billing, then a summary since the subscription's currentPeriodStart. These
// GETs do not activate fingerprint sessions or change inference breakers.
func (c *Client) Usage(ctx context.Context, apiKey string) (*AccountUsage, error) {
	out := &AccountUsage{OrgLimits: []OrgLimit{}, Errors: []UsageError{}}
	var who struct {
		Org *struct {
			ID string `json:"id"`
		} `json:"org"`
		OrgLimits []OrgLimit `json:"orgLimits"`
	}
	errWho := c.getJSON(ctx, "/alpha/whoami?limits=1", apiKey, &who)
	params := url.Values{}
	if errWho == nil {
		if who.Org != nil && who.Org.ID != "" {
			params.Set("orgId", who.Org.ID)
		}
		if who.OrgLimits != nil {
			out.OrgLimits = who.OrgLimits
		}
	}
	endpoint := func(path string) string {
		if len(params) == 0 {
			return path
		}
		return path + "?" + params.Encode()
	}
	var credits struct {
		Credits      *CreditBalance `json:"credits"`
		WindowLimits *WindowLimits  `json:"windowLimits"`
	}
	var subscription struct {
		Data *Subscription `json:"data"`
	}
	var errCredits, errSubscription error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errCredits = c.getJSON(ctx, endpoint("/alpha/billing/credits"), apiKey, &credits)
	}()
	go func() {
		defer wg.Done()
		errSubscription = c.getJSON(ctx, endpoint("/alpha/billing/subscriptions"), apiKey, &subscription)
	}()
	wg.Wait()
	if errCredits == nil {
		out.Credits, out.WindowLimits = credits.Credits, credits.WindowLimits
	}
	if errSubscription == nil {
		out.Subscription = subscription.Data
		if subscription.Data != nil && subscription.Data.CurrentPeriodStart != "" {
			params.Set("since", subscription.Data.CurrentPeriodStart)
		}
	}
	var summary UsageSummary
	errSummary := c.getJSON(ctx, endpoint("/alpha/usage/summary"), apiKey, &summary)
	if errSummary == nil {
		out.Summary = &summary
	}
	for _, part := range []struct {
		name string
		err  error
	}{
		{"identity", errWho}, {"credits", errCredits}, {"subscription", errSubscription}, {"summary", errSummary},
	} {
		if part.err == nil {
			continue
		}
		failure := UsageError{Section: part.name, Message: "Upstream usage data unavailable"}
		var ae *APIError
		if errors.As(part.err, &ae) {
			failure.Status = ae.Status
		}
		out.Errors = append(out.Errors, failure)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Keep cached display data bounded even if optional upstream strings or
	// organization limits are unexpectedly large. Never retain raw payloads.
	if len(out.OrgLimits) > 128 {
		out.OrgLimits = out.OrgLimits[:128:128]
	}
	limits := append([]OrgLimit(nil), out.OrgLimits...)
	for i := range limits {
		l := &limits[i]
		l.Scope, l.Model, l.ModelLabel = usageLabel(l.Scope), usageLabel(l.Model), usageLabel(l.ModelLabel)
		l.ResetInterval = usageLabel(l.ResetInterval)
		if len(l.ResetAt) > 128 {
			l.ResetAt = nil
		}
	}
	out.OrgLimits = limits
	if sub := out.Subscription; sub != nil {
		sub.PlanID, sub.Status = usageLabel(sub.PlanID), usageLabel(sub.Status)
		sub.CurrentPeriodStart, sub.CurrentPeriodEnd = usageLabel(sub.CurrentPeriodStart), usageLabel(sub.CurrentPeriodEnd)
	}
	if out.Summary != nil {
		out.Summary.PeriodBasis = usageLabel(out.Summary.PeriodBasis)
	}
	out.FetchedAt = time.Now()
	return out, nil
}

func usageLabel(value string) string {
	if len(value) > 160 {
		return "[value too long]"
	}
	return strings.Clone(value)
}
