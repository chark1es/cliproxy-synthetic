# cliproxy-synthetic

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that makes [Synthetic](https://synthetic.new) a first-class provider instead of a bare OpenAI-compatible endpoint.

| | OpenAI-compatible entry | this plugin |
| --- | --- | --- |
| Shows up as an account in the management panel | no | yes |
| Per-account quota in the panel (weekly credits and search) | no | yes |
| Model list discovered from Synthetic | hand-written | automatic, per account |
| Anthropic Messages API (`/v1/messages`, streaming, `count_tokens`) | translated | native Synthetic endpoint |
| Synthetic web search | no | management route |
| Add an account without editing config | no | management route, key validated first |

## Requirements

- CLIProxyAPI v8 (developed and tested against **v8.0.43**) with dynamic plugins enabled.
- Linux, `amd64` or `arm64`. Releases are built in a Debian 12 container, so they link against glibc 2.34 and run on the official `eceasy/cli-proxy-api` image and anything newer.

## Install

1. Enable plugins in `config.yaml`:

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     configs:
       synthetic:
         enabled: true
         priority: 1
   ```

2. Download `synthetic_linux_<arch>.so` from the [latest release](../../releases/latest) and verify it against `checksums.txt`. Save it as `synthetic.so` in `<plugins dir>/linux/<arch>/` (or directly in `<plugins dir>/`). The plugin ID is the file name without the extension, so it must be `synthetic` for the `plugins.configs.synthetic` block above to apply.

   With the official Docker image the plugins directory is `/CLIProxyAPI/plugins`; mount a volume there so the library survives redeploys.

   Release zips (`synthetic_<version>_linux_<arch>.zip`, library at the zip root, plus `checksums.txt`) follow CLIProxyAPI's plugin-store layout.

3. Restart CLIProxyAPI. The log shows `plugin registered plugin_id=synthetic`, and `GET /v0/management/plugins` lists it with `supports_oauth` and `supports_quota` true.

> **Remove any `openai-compatibility` entry for Synthetic.** When the same provider is configured both ways, the built-in executor wins over a plugin executor, so the plugin would never be used.

## Add an account

Accounts are API keys. Either call the management API (the key is checked against Synthetic's free `/v2/quotas` endpoint first, so a typo is rejected instead of saved):

```bash
curl -X POST https://proxy.example/v0/management/synthetic/accounts \
  -H "Authorization: Bearer $MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"api_key": "syn_...", "label": "work"}'
```

or drop an auth file into the auth directory (`oauth.auth-dir`, `~/.cli-proxy-api` by default), for example `synthetic-work.json`:

```json
{ "type": "synthetic", "api_key": "syn_...", "label": "work" }
```

Optional fields are the usual CLIProxyAPI auth-file ones: `disabled`, `prefix`, `proxy_url`, `priority`. `apiKey` and `key` are accepted as aliases of `api_key`.

With more than one account, CLIProxyAPI's normal credential routing applies.

## Use it

Models are discovered from each account (`syn:large:text`, `hf:zai-org/GLM-5.3`, ...) and appear in `/v1/models`. Clients keep using the proxy's own API key:

```bash
# OpenAI chat completions
curl https://proxy.example/v1/chat/completions \
  -H "Authorization: Bearer $PROXY_API_KEY" -H "Content-Type: application/json" \
  -d '{"model": "syn:small:text", "messages": [{"role": "user", "content": "hello"}]}'

# Anthropic Messages
curl https://proxy.example/v1/messages \
  -H "x-api-key: $PROXY_API_KEY" -H "anthropic-version: 2023-06-01" -H "Content-Type: application/json" \
  -d '{"model": "syn:small:text", "max_tokens": 256, "messages": [{"role": "user", "content": "hello"}]}'
```

The executor handles chat completions and Anthropic Messages natively, with streaming. The Responses API and other client formats are translated by CLIProxyAPI itself.

## Management routes

All routes are under `/v0/management` and need the management key. None of them ever return a full API key; keys are shown as `...abcd`.

| Route | Purpose |
| --- | --- |
| `GET /synthetic/accounts` | List Synthetic accounts. |
| `POST /synthetic/accounts` | Add an account: `{"api_key", "label"?, "name"?}`. |
| `GET /synthetic/quotas[?auth_index=]` | Raw and normalized usage for each enabled account. |
| `POST /synthetic/search` | `{"query", "auth_index"?}`. Runs Synthetic web search; without `auth_index` it rotates across accounts. Search is limited per hour by your plan. |
| `GET /plugins/synthetic/quota?auth_index=` | The standard plugin quota route used by the management panel. |

## Quota reporting

Synthetic's [`/v2/quotas`](https://dev.synthetic.new/docs/synthetic/quotas) is mapped to the panel's quota format:

- **Weekly credits**: percent remaining, with a 2% recharge every 202 minutes (3h22m), as documented in [Synthetic’s rate limits](https://synthetic.new/rate-limits). The displayed time is the next incremental recharge, not a full reset. When supplied, the API's `nextRegenPercent` is used for the recharge amount.
- **Search**: hourly search usage, only when returned by the API.

When weekly data is present, the old subscription request bucket is hidden. Subscription-only responses (as still shown in the API docs) display a generic usage allowance without assuming a request count or a five-hour window. Quota lookups do not count against your subscription.

The new plans are Go ($9/month), Standard ($30/month), and Big ($90/month). Weekly credits are consumed at model-specific rates; the plugin uses the API's remaining percentage rather than estimating tokens, plan size, or migration bonuses. There are no packs, five-hour limits, or fixed concurrency caps in the new plans. Priority and service throttling are handled by Synthetic.

Fast Mode is enabled on Synthetic's usage page for Big subscribers. Infinite Mode uses the `infinite:small:vision` model alias and requires streaming; use it if returned by your account's model list. Model discovery and request forwarding preserve these aliases. See [Synthetic's model documentation](https://dev.synthetic.new/docs/api/models).

## Configuration

Under `plugins.configs.synthetic`, next to the host's `enabled` and `priority`:

| Key | Default | Meaning |
| --- | --- | --- |
| `base_url` | `https://api.synthetic.new` | API origin. Change it only for a mirror or test server. |
| `models_cache_seconds` | `300` | How long a discovered model list is reused. A stale list is kept if Synthetic is unreachable. |

## Security notes

- Native plugins run **inside the proxy process** with its privileges. Only install builds you trust; verify `checksums.txt`, or build from source.
- The proxy client's `Authorization` header is never forwarded upstream. Only a fixed set of headers (`Content-Type`, `Accept`, `Anthropic-Version`, `Anthropic-Beta`) is passed on, and each request is signed with the selected account's key.
- The raw HTTP bridge only signs requests to the configured `base_url` origin.
- Management routes are authenticated-only. No browser resources are registered, because those are served without management authentication.

## Known limitations

- A thinking-suffix model name such as `model(high)` is sent upstream as plain `model`; pass `reasoning_effort` in the request body to control reasoning.
- `count_tokens` exists only for the Anthropic format.
- Search is exposed through the management API only: plugins cannot add routes that accept ordinary proxy client keys.
- When a Responses API stream is cut off by `max_output_tokens` during reasoning, CLIProxyAPI reports `upstream stream closed before a terminal event`. This also happens with Synthetic configured as a built-in OpenAI-compatible provider, so it comes from the host's translation layer, not from this plugin.

## Build from source

```bash
go test -race ./...
scripts/build.sh 0.1.0 dist     # needs Docker; builds dist/synthetic.so in a Debian 12 Go container
scripts/verify.sh dist/synthetic.so
```

The plugin is a `c-shared` Go library implementing CLIProxyAPI's standard plugin ABI (schema version 6): auth provider, model provider, executor, quota provider and management API. It depends only on CLIProxyAPI's public `sdk/pluginabi` and `sdk/pluginapi` packages.

## License

[MIT](LICENSE)
