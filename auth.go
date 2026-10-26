package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const loginUnsupportedMessage = "Synthetic uses API keys, not an interactive login. " +
	"Add an account with POST /v0/management/synthetic/accounts or drop a synthetic-*.json auth file " +
	`({"type":"synthetic","api_key":"..."}) into the auth directory.`

// account is the credential stored in a synthetic auth file.
type account struct {
	Type     string `json:"type"`
	APIKey   string `json:"api_key"`
	Label    string `json:"label"`
	Email    string `json:"email"`
	Disabled bool   `json:"disabled"`
	Prefix   string `json:"prefix"`
	ProxyURL string `json:"proxy_url"`
}

// parseAccount reads a synthetic auth file. api_key is canonical; apiKey and
// key are accepted so pasted snippets from other tools keep working.
func parseAccount(raw []byte) (account, error) {
	var acct account
	if errUnmarshal := json.Unmarshal(raw, &acct); errUnmarshal != nil {
		return account{}, fmt.Errorf("decode synthetic auth file: %w", errUnmarshal)
	}
	if acct.APIKey == "" {
		var alt struct {
			APIKey string `json:"apiKey"`
			Key    string `json:"key"`
		}
		if json.Unmarshal(raw, &alt) == nil {
			acct.APIKey = alt.APIKey
			if acct.APIKey == "" {
				acct.APIKey = alt.Key
			}
		}
	}
	acct.APIKey = strings.TrimSpace(acct.APIKey)
	acct.Type = strings.ToLower(strings.TrimSpace(acct.Type))
	return acct, nil
}

// requireKey returns the account's key or a 401-shaped error for the host.
func requireKey(storage []byte) (account, error) {
	acct, errParse := parseAccount(storage)
	if errParse != nil {
		return account{}, pluginabi.NewError("invalid_auth", errParse.Error(), http.StatusUnauthorized)
	}
	if acct.APIKey == "" {
		return account{}, pluginabi.NewError("invalid_auth", "synthetic auth file has no api_key", http.StatusUnauthorized)
	}
	return acct, nil
}

func bearer(key string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + key}}
}

func handleAuthParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth parse request: %w", errUnmarshal)
	}
	// With no provider hint the host offers every auth file to every auth
	// plugin, so decline quickly unless the file is ours.
	if p := strings.ToLower(strings.TrimSpace(req.Provider)); p != "" && p != providerID {
		return okEnvelope(pluginapi.AuthParseResponse{})
	}
	acct, errParse := parseAccount(req.RawJSON)
	if errParse != nil || acct.Type != providerID {
		return okEnvelope(pluginapi.AuthParseResponse{})
	}
	if acct.APIKey == "" {
		return nil, pluginabi.NewError("invalid_auth", fmt.Sprintf("%s has type %q but no api_key", req.FileName, providerID))
	}
	label := firstNonEmpty(acct.Label, acct.Email, strings.TrimSuffix(req.FileName, ".json"), "synthetic")
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:    providerID,
			FileName:    req.FileName,
			Label:       label,
			Prefix:      acct.Prefix,
			ProxyURL:    acct.ProxyURL,
			Disabled:    acct.Disabled,
			StorageJSON: req.RawJSON,
		},
	})
}

// handleAuthRefresh is a no-op: API keys do not expire. Every empty field in
// the response is filled from the existing auth by the host; the long
// NextRefreshAfter keeps the host from asking again every few minutes.
func handleAuthRefresh() ([]byte, error) {
	return okEnvelope(pluginapi.AuthRefreshResponse{
		NextRefreshAfter: time.Now().Add(30 * 24 * time.Hour).UTC(),
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// accountFileName derives a stable auth file name. A label gives a readable
// name; otherwise a short hash of the key keeps names unique without leaking it.
func accountFileName(label, apiKey string) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	if slug == "" {
		sum := sha256.Sum256([]byte(apiKey))
		slug = "key-" + hex.EncodeToString(sum[:])[:8]
	}
	return "synthetic-" + slug + ".json"
}

// maskKey hides all but the last four characters of an API key.
func maskKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return "..." + key[len(key)-4:]
}
