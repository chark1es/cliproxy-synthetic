package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaWindow is one counted allowance, e.g. subscription requests per period.
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
		NextRegenAt      string  `json:"nextRegenAt"`
		PercentRemaining float64 `json:"percentRemaining"`
		NextRegenPercent float64 `json:"nextRegenPercent"`
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

// quotaResponse converts Synthetic's quota document into the normalized shape
// the management UI renders.
func quotaResponse(q synthQuotas) pluginapi.QuotaFetchResponse {
	resp := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "Synthetic"}}

	var requestBuckets []pluginapi.QuotaBucket
	if b := windowBucket("subscription", q.Subscription); b != nil {
		requestBuckets = append(requestBuckets, *b)
		resp.Summary = append(resp.Summary,
			pluginapi.QuotaMetric{Key: "requests_used", Label: "Requests used", Value: q.Subscription.Requests, Unit: "requests", Format: "number"},
			pluginapi.QuotaMetric{Key: "requests_limit", Label: "Request limit", Value: q.Subscription.Limit, Unit: "requests", Format: "number"},
		)
	}
	if len(requestBuckets) > 0 {
		resp.Groups = append(resp.Groups, pluginapi.QuotaGroup{DisplayName: "Requests", Buckets: requestBuckets})
	}

	if q.Search != nil {
		if b := windowBucket("hourly", q.Search.Hourly); b != nil {
			resp.Groups = append(resp.Groups, pluginapi.QuotaGroup{DisplayName: "Search", Buckets: []pluginapi.QuotaBucket{*b}})
			resp.Summary = append(resp.Summary,
				pluginapi.QuotaMetric{Key: "search_used", Label: "Searches this hour", Value: q.Search.Hourly.Requests, Unit: "requests", Format: "number"},
			)
		}
	}

	if w := q.PlanWeeklyLimit; w != nil {
		resp.Groups = append(resp.Groups, pluginapi.QuotaGroup{
			DisplayName: "Weekly plan limit",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            "weekly",
				RemainingFraction: clamp01(w.PercentRemaining / 100),
				ResetTime:         w.NextRegenAt,
				Description:       fmt.Sprintf("%.0f%% remaining, regenerates %.0f%% at the reset time", w.PercentRemaining, w.NextRegenPercent),
			}},
		})
		resp.Summary = append(resp.Summary,
			pluginapi.QuotaMetric{Key: "weekly_remaining", Label: "Weekly limit remaining", Value: w.PercentRemaining, Unit: "%", Format: "number"},
		)
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
