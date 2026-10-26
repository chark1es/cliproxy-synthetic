package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// routePrefix is where this plugin's routes live under /v0/management.
const routePrefix = "/synthetic"

type managementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRegistration struct {
	Routes []pluginapi.ManagementRoute `json:"routes,omitempty"`
}

// handleManagementRegister declares authenticated Management API routes only.
// No browser resources are registered: those are served without management
// authentication, and these routes read or write credentials.
func handleManagementRegister() ([]byte, error) {
	route := func(method, path, description string) pluginapi.ManagementRoute {
		return pluginapi.ManagementRoute{Method: method, Path: routePrefix + path, Description: description}
	}
	return okEnvelope(managementRegistration{Routes: []pluginapi.ManagementRoute{
		route(http.MethodGet, "/accounts", "List Synthetic accounts (keys are masked)."),
		route(http.MethodPost, "/accounts", "Add a Synthetic account from an API key after validating it."),
		route(http.MethodGet, "/quotas", "Show subscription, search and weekly plan usage for each account."),
		route(http.MethodPost, "/search", "Run a Synthetic web search with an account's key."),
	}})
}

func jsonResponse(status int, v any) ([]byte, error) {
	body, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	})
}

func errorResponse(status int, message string) ([]byte, error) {
	return jsonResponse(status, map[string]any{"error": message})
}

// failureResponse reports an upstream or host failure with the most specific
// HTTP status available.
func failureResponse(err error) ([]byte, error) {
	if pe, ok := err.(*pluginabi.Error); ok && pe.HTTPStatus >= 400 && pe.HTTPStatus < 600 {
		return errorResponse(pe.HTTPStatus, pe.Message)
	}
	return errorResponse(http.StatusBadGateway, err.Error())
}

func handleManagementRequest(request []byte) ([]byte, error) {
	var req managementRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
	}
	path := req.Path
	if i := strings.Index(path, routePrefix+"/"); i >= 0 {
		path = path[i+len(routePrefix):]
	}
	method := strings.ToUpper(req.Method)
	switch {
	case path == "/accounts" && method == http.MethodGet:
		return routeListAccounts()
	case path == "/accounts" && method == http.MethodPost:
		return routeAddAccount(req)
	case path == "/quotas" && method == http.MethodGet:
		return routeQuotas(req)
	case path == "/search" && method == http.MethodPost:
		return routeSearch(req)
	}
	return errorResponse(http.StatusNotFound, "no such synthetic route: "+method+" "+req.Path)
}

// synthAccount pairs a host credential entry with its parsed key.
type synthAccount struct {
	File authFile
	Acct account
}

func isSyntheticFile(f authFile) bool {
	return strings.EqualFold(f.Provider, providerID) || strings.EqualFold(f.Type, providerID)
}

// loadAccounts returns every readable, non-disabled Synthetic credential. A
// credential with no backing file or key is skipped, not fatal.
func loadAccounts(includeDisabled bool) ([]synthAccount, error) {
	files, errList := hostAuthList()
	if errList != nil {
		return nil, errList
	}
	var out []synthAccount
	for _, f := range files {
		if !isSyntheticFile(f) || (f.Disabled && !includeDisabled) {
			continue
		}
		raw, errGet := hostAuthJSON(f.AuthIndex)
		if errGet != nil {
			continue
		}
		acct, errParse := parseAccount(raw)
		if errParse != nil || acct.APIKey == "" {
			continue
		}
		out = append(out, synthAccount{File: f, Acct: acct})
	}
	return out, nil
}

func routeListAccounts() ([]byte, error) {
	accounts, errLoad := loadAccounts(true)
	if errLoad != nil {
		return failureResponse(errLoad)
	}
	type entry struct {
		AuthIndex string `json:"auth_index"`
		Name      string `json:"name"`
		Label     string `json:"label,omitempty"`
		Status    string `json:"status,omitempty"`
		Disabled  bool   `json:"disabled"`
		KeyHint   string `json:"key_hint"`
	}
	list := make([]entry, 0, len(accounts))
	for _, a := range accounts {
		list = append(list, entry{
			AuthIndex: a.File.AuthIndex,
			Name:      a.File.Name,
			Label:     firstNonEmpty(a.Acct.Label, a.File.Label),
			Status:    a.File.Status,
			Disabled:  a.File.Disabled,
			KeyHint:   maskKey(a.Acct.APIKey),
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{"accounts": list})
}

func routeAddAccount(req managementRequest) ([]byte, error) {
	var body struct {
		APIKey string `json:"api_key"`
		Label  string `json:"label"`
		Name   string `json:"name"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return errorResponse(http.StatusBadRequest, "request body must be JSON: {\"api_key\": \"...\", \"label\": \"...\"}")
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		return errorResponse(http.StatusBadRequest, "api_key is required")
	}
	// Quotas are free to read, so use them to reject a wrong key up front.
	if _, errCheck := fetchQuotas(req.HostCallbackID, account{APIKey: key}); errCheck != nil {
		if pe, ok := errCheck.(*pluginabi.Error); ok && (pe.HTTPStatus == http.StatusUnauthorized || pe.HTTPStatus == http.StatusForbidden) {
			return errorResponse(http.StatusBadRequest, "Synthetic rejected this API key: "+pe.Message)
		}
		return failureResponse(errCheck)
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = accountFileName(body.Label, key)
	} else {
		name = strings.TrimSuffix(name, ".json") + ".json"
		if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
			return errorResponse(http.StatusBadRequest, "name must be a plain file name")
		}
	}
	stored, errMarshal := json.Marshal(map[string]any{
		"type":    providerID,
		"api_key": key,
		"label":   strings.TrimSpace(body.Label),
	})
	if errMarshal != nil {
		return nil, errMarshal
	}
	saved, errSave := hostAuthSave(name, stored)
	if errSave != nil {
		return failureResponse(errSave)
	}
	return jsonResponse(http.StatusCreated, map[string]any{"name": saved.Name, "path": saved.Path, "key_hint": maskKey(key)})
}

type accountQuota struct {
	AuthIndex string                        `json:"auth_index"`
	Name      string                        `json:"name"`
	Label     string                        `json:"label,omitempty"`
	Quotas    *synthQuotas                  `json:"quotas,omitempty"`
	Summary   *pluginapi.QuotaFetchResponse `json:"normalized,omitempty"`
	Error     string                        `json:"error,omitempty"`
}

func routeQuotas(req managementRequest) ([]byte, error) {
	want := strings.TrimSpace(req.Query.Get("auth_index"))
	accounts, errLoad := loadAccounts(false)
	if errLoad != nil {
		return failureResponse(errLoad)
	}
	results := make([]accountQuota, 0, len(accounts))
	for _, a := range accounts {
		if want != "" && a.File.AuthIndex != want {
			continue
		}
		item := accountQuota{AuthIndex: a.File.AuthIndex, Name: a.File.Name, Label: firstNonEmpty(a.Acct.Label, a.File.Label)}
		q, errFetch := fetchQuotas(req.HostCallbackID, a.Acct)
		if errFetch != nil {
			item.Error = errFetch.Error()
		} else {
			normalized := quotaResponse(q)
			item.Quotas = &q
			item.Summary = &normalized
		}
		results = append(results, item)
	}
	if want != "" && len(results) == 0 {
		return errorResponse(http.StatusNotFound, "no enabled synthetic account with auth_index "+want)
	}
	return jsonResponse(http.StatusOK, map[string]any{"accounts": results})
}

var searchCursor atomic.Uint64

// pickAccount chooses the account named by auth_index, or rotates through the
// enabled accounts so repeated searches spread across them.
func pickAccount(accounts []synthAccount, authIndex string) (synthAccount, bool) {
	if authIndex != "" {
		for _, a := range accounts {
			if a.File.AuthIndex == authIndex {
				return a, true
			}
		}
		return synthAccount{}, false
	}
	if len(accounts) == 0 {
		return synthAccount{}, false
	}
	return accounts[int(searchCursor.Add(1)-1)%len(accounts)], true
}

func routeSearch(req managementRequest) ([]byte, error) {
	var body struct {
		Query     string `json:"query"`
		AuthIndex string `json:"auth_index"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil || strings.TrimSpace(body.Query) == "" {
		return errorResponse(http.StatusBadRequest, "request body must be JSON with a non-empty \"query\"")
	}
	accounts, errLoad := loadAccounts(false)
	if errLoad != nil {
		return failureResponse(errLoad)
	}
	acct, ok := pickAccount(accounts, strings.TrimSpace(body.AuthIndex))
	if !ok {
		return errorResponse(http.StatusNotFound, "no enabled synthetic account available")
	}
	payload, errMarshal := json.Marshal(map[string]string{"query": strings.TrimSpace(body.Query)})
	if errMarshal != nil {
		return nil, errMarshal
	}
	header := bearer(acct.Acct.APIKey)
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json")
	result, errDo := hostHTTPDo(httpCall{
		CallbackID: req.HostCallbackID,
		Method:     http.MethodPost,
		URL:        cfg().searchURL(),
		Header:     header,
		Body:       payload,
	})
	if errDo != nil {
		return errorResponse(http.StatusBadGateway, errDo.Error())
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: result.Status,
		Headers:    http.Header{"Content-Type": []string{contentType(result.Header, "application/json")}},
		Body:       result.Body,
	})
}
