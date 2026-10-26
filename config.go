package main

import (
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

const (
	defaultBaseURL            = "https://api.synthetic.new"
	defaultModelsCacheSeconds = 300
)

// config is the plugin's slice of plugins.configs.synthetic.
type config struct {
	BaseURL            string `yaml:"base_url"`
	ModelsCacheSeconds int    `yaml:"models_cache_seconds"`
}

var currentConfig atomic.Pointer[config]

func defaultConfig() *config {
	return &config{BaseURL: defaultBaseURL, ModelsCacheSeconds: defaultModelsCacheSeconds}
}

// cfg returns the active configuration, never nil.
func cfg() *config {
	if c := currentConfig.Load(); c != nil {
		return c
	}
	return defaultConfig()
}

// parseConfig decodes the YAML mapping the host passes at plugin.register and
// plugin.reconfigure. Unknown keys (enabled, priority, ...) belong to the host.
func parseConfig(raw []byte) (*config, error) {
	c := defaultConfig()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if errUnmarshal := yaml.Unmarshal(raw, c); errUnmarshal != nil {
			return nil, fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	parsed, errParse := url.Parse(base)
	if errParse != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("base_url %q must be an absolute http(s) URL", c.BaseURL)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("base_url must not contain credentials, a query or a fragment")
	}
	c.BaseURL = base
	if c.ModelsCacheSeconds <= 0 {
		c.ModelsCacheSeconds = defaultModelsCacheSeconds
	}
	return c, nil
}

func applyConfig(raw []byte) error {
	c, errParse := parseConfig(raw)
	if errParse != nil {
		return errParse
	}
	currentConfig.Store(c)
	return nil
}

func (c *config) openAIURL(path string) string    { return c.BaseURL + "/openai/v1" + path }
func (c *config) anthropicURL(path string) string { return c.BaseURL + "/anthropic/v1" + path }
func (c *config) quotasURL() string               { return c.BaseURL + "/v2/quotas" }
func (c *config) searchURL() string               { return c.BaseURL + "/v2/search" }

// sameOrigin reports whether rawURL points at the configured Synthetic origin.
// The executor's raw HTTP bridge refuses anything else so a request can never
// carry an account's API key to a different host.
func (c *config) sameOrigin(rawURL string) bool {
	target, errTarget := url.Parse(rawURL)
	base, errBase := url.Parse(c.BaseURL)
	if errTarget != nil || errBase != nil {
		return false
	}
	return strings.EqualFold(target.Scheme, base.Scheme) && strings.EqualFold(target.Host, base.Host) && target.User == nil
}
