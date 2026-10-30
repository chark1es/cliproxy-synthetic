package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaWindow is an allowance reported by the API. Subscription values are
// usage units; search values are request counts.
type quotaWindow struct {
	Limit    float64 `json:"limit"`
	Requests float64 `json:"requests"`
	RenewsAt string  `json:"renewsAt"`
}

// synthQuotas mirrors GET /v2/quotas. Every section is optional because plans
// differ and Synthetic adds fields over time.
type synthQuotas struct {
	Subscription *quotaWindow `json:"subscription"`
	Search       *struct {
		Hourly *quotaWindow `json:"hourly"`
	} `json:"search"`
	PlanWeeklyLimit *struct {
		NextRegenAt      string   `json:"nextRegenAt"`
		PercentRemaining float64  `json:"percentRemaining"`
		NextRegenPercent *float64 `json:"nextRegenPercent,omitempty"`
	} `json:"planWeeklyLimit"`
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func windowBucket(label string, w *quotaWindow) *pluginapi.QuotaBucket {
	if w == nil || w.Limit <= 0 {
		return nil
	}
	return &pluginapi.QuotaBucket{
		Window:            label,
		RemainingFraction: clamp01((w.Limit - w.Requests) / w.Limit),
		ResetTime:         w.RenewsAt,
		Description:       fmt.Sprintf("%.0f of %.0f requests used", w.Requests, w.Limit),
	}
}

const weeklyRechargeInterval = 202 * time.Minute

// weeklyFullRegenAt anchors the estimate at the API's next tick. That tick can
// be partial; subsequent ticks restore the documented 2% of weekly credits.
func weeklyFullRegenAt(percentRemaining float64, nextRegenAt string, nextRecharge float64) string {
	remaining := math.Max(0, math.Min(100, percentRemaining))
	if remaining >= 100 || nextRecharge <= 0 {
		return ""
	}
	next, err := time.Parse(time.RFC3339Nano, nextRegenAt)
	if err != nil {
		return ""
	}
	ticksAfterNext := math.Ceil(math.Max(0, 100-remaining-nextRecharge) / 2)
	return next.Add(time.Duration(ticksAfterNext) * weeklyRechargeInterval).UTC().Format(time.RFC3339Nano)
}

// quotaResponse converts Synthetic's quota document into the normalized shape
// the management UI renders.
func quotaResponse(q synthQuotas) pluginapi.QuotaFetchResponse {
	resp := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "Synthetic"}}

	if w := q.PlanWeeklyLimit; w != nil {
		// A recharge is incremental, not a reset to a full weekly balance.
		// Prefer the API's tick amount; older responses may omit it.
		recharge := 2.0
		if w.NextRegenPercent != nil {
			recharge = *w.NextRegenPercent
		}
		fullRegenAt := weeklyFullRegenAt(w.PercentRemaining, w.NextRegenAt, recharge)
		description := fmt.Sprintf("%g%% weekly credits remaining; recharges 2%% every 202 minutes", w.PercentRemaining)
		if w.PercentRemaining >= 100 {
			description += "; fully regenerated"
		} else {
			if _, err := time.Parse(time.RFC3339Nano, w.NextRegenAt); err == nil {
				description += fmt.Sprintf("; next recharge %g%% at %s", recharge, w.NextRegenAt)
			}
			if fullRegenAt != "" {
				description += "; fully regenerated at the indicated time, assuming no further usage"
			}
		}
		resp.Groups = append(resp.Groups, pluginapi.QuotaGroup{
			DisplayName: "Weekly credits",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            "weekly",
				RemainingFraction: clamp01(w.PercentRemaining / 100),
				ResetTime:         fullRegenAt,
				Description:       description,
			}},
		})
		resp.Summary = append(resp.Summary,
			pluginapi.QuotaMetric{Key: "weekly_remaining", Label: "Weekly credits remaining", Value: w.PercentRemaining, Unit: "%", Format: "number"},
		)
	} else if w := q.Subscription; w != nil && w.Limit > 0 {
		// The published API example still uses subscription-only responses.
		// Keep that format readable without interpreting usage as a token or
		// request count, or exposing a second limit alongside weekly credits.
		resp.Groups = append(resp.Groups, pluginapi.QuotaGroup{
			DisplayName: "Subscription usage",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            "subscription",
				RemainingFraction: clamp01((w.Limit - w.Requests) / w.Limit),
				ResetTime:         w.RenewsAt,
				Description:       fmt.Sprintf("%g of %g usage units consumed", w.Requests, w.Limit),
			}},
		})
		resp.Summary = append(resp.Summary,
			pluginapi.QuotaMetric{Key: "subscription_used", Label: "Subscription usage", Value: w.Requests, Unit: "usage", Format: "number"},
			pluginapi.QuotaMetric{Key: "subscription_limit", Label: "Subscription allowance", Value: w.Limit, Unit: "usage", Format: "number"},
		)
	}

	if q.Search != nil {
		if b := windowBucket("hourly", q.Search.Hourly); b != nil {
			resp.Groups = append(resp.Groups, pluginapi.QuotaGroup{DisplayName: "Search", Buckets: []pluginapi.QuotaBucket{*b}})
			resp.Summary = append(resp.Summary,
				pluginapi.QuotaMetric{Key: "search_used", Label: "Searches this hour", Value: q.Search.Hourly.Requests, Unit: "requests", Format: "number"},
			)
		}
	}

	return resp
}

// fetchQuotas reads the account's current usage. Synthetic documents /quotas
// calls as not counting against the subscription.
func fetchQuotas(callbackID string, acct account) (synthQuotas, error) {
	result, errDo := hostHTTPDo(httpCall{
		CallbackID: callbackID,
		Method:     http.MethodGet,
		URL:        cfg().quotasURL(),
		Header:     bearer(acct.APIKey),
	})
	if errDo != nil {
		return synthQuotas{}, pluginabi.NewError("upstream_unreachable", errDo.Error(), http.StatusBadGateway)
	}
	if result.Status < 200 || result.Status >= 300 {
		return synthQuotas{}, upstreamError(result.Status, result.Header, result.Body)
	}
	var q synthQuotas
	if errUnmarshal := json.Unmarshal(result.Body, &q); errUnmarshal != nil {
		return synthQuotas{}, fmt.Errorf("decode synthetic quotas: %w", errUnmarshal)
	}
	return q, nil
}

type quotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleQuotaFetch(request []byte) ([]byte, error) {
	var req quotaFetchRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode quota request: %w", errUnmarshal)
	}
	if p := strings.ToLower(strings.TrimSpace(req.Provider)); p != "" && p != providerID {
		return nil, pluginabi.NewError("unsupported_provider", "synthetic quota provider cannot serve "+req.Provider, http.StatusBadRequest)
	}
	acct, errKey := requireKey(req.StorageJSON)
	if errKey != nil {
		return nil, errKey
	}
	q, errFetch := fetchQuotas(req.HostCallbackID, acct)
	if errFetch != nil {
		return nil, errFetch
	}
	return okEnvelope(quotaResponse(q))
}
