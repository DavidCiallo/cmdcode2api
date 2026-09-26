# cmdcode2api

[中文说明](README.zh-CN.md)

`cmdcode2api` is a small OpenAI-compatible gateway for [Command Code](https://commandcode.ai/). OpenAI-style clients call the familiar `/v1/chat/completions` and `/v1/models` endpoints, and the gateway forwards requests to Command Code — rotating across multiple accounts, tracking usage, and serving a built-in admin WebUI.

## Features

- OpenAI-compatible `POST /v1/chat/completions` (streaming and non-streaming) and `GET /v1/models`
- Multiple Command Code accounts with round-robin rotation and automatic failover (`401`/`403`/`429`/5xx), including per-account 429 cooldown
- Multiple local client API keys with per-key request and token accounting
- Command Code quota dashboard: 5-hour / weekly / estimated monthly progress bars, credit balances, plan and billing period, refreshed in the background every 5 minutes
- Embedded single-file WebUI: usage dashboard, account/key management, model exposure editor, live settings, and log tail
- Browser OAuth helper for obtaining a Command Code API key (CLI or WebUI); a CLI run prints the key for `COMMANDCODE_API_KEY`
- Local bearer-token auth for clients, separate admin password for the WebUI
- Stateless in-memory operation: no config file and no data volume, so nothing is written to disk
- Usage counters (global, per-account, per-client-key) and cached quota snapshots held in memory only, reset to zero on restart
- Base64 `image_url` conversion to Command Code image blocks; CORS enabled for local UI clients
- `GET /health` and `GET /usage` endpoints

## Quick start

```bash
go build -o cmdcode2api ./cmd/cmdcode2api
./cmdcode2api
```

No config file is created — the server builds its configuration in memory and starts straight away, printing a generated client key and WebUI admin password once. Add a Command Code account (next section), then point any OpenAI client at the gateway:

```bash
curl http://localhost:11434/v1/chat/completions \
  -H "Authorization: Bearer <local-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek/deepseek-v4-flash",
    "messages": [
      {"role": "user", "content": "Hello"}
    ],
    "stream": false
  }'
```

The server starts fine with zero accounts — the WebUI, client keys, and settings all work, and chat requests return `503 no_accounts` until one is added. Adding the first account from the WebUI fetches the model catalog immediately.

The gateway keeps no state on disk: accounts, client keys, and settings live in memory for the current process only and are lost on restart. Set `CLIENT_API_KEY` and `ADMIN_PASSWORD` in the environment to keep those credentials stable across restarts, and `COMMANDCODE_API_KEY` to seed an upstream account.

## Docker

Prebuilt multi-arch images are published to GHCR by CI on every master push (`latest`) and every `v*` tag. The image is stateless — there is no data volume and nothing to mount:

```bash
docker run -d --name cmdcode2api -p 11434:11434 \
  -e ADMIN_PASSWORD=<webui-admin-password> \
  -e CLIENT_API_KEY=<local-client-key> \
  -e COMMANDCODE_API_KEY=<command-code-api-key> \
  ghcr.io/peach0x33a/cmdcode2api:latest
```

Every variable is optional: without `CLIENT_API_KEY` and `ADMIN_PASSWORD` the container still serves, generating both and printing them once to the logs (`docker logs cmdcode2api`). Without `COMMANDCODE_API_KEY` it starts with no accounts, and chat requests return `503` until an account is added in the WebUI. `docker-compose.yml` sets these variables from `${...}` with the same defaults. To build the image locally, use `docker build -t cmdcode2api .` — networks that cannot reach proxy.golang.org can pass `--build-arg GOPROXY=https://goproxy.cn,direct`.

## Adding Command Code accounts

### WebUI (simplest)

Open `/webui`, go to the **Accounts** tab, and paste a Command Code API key. This works regardless of container networking or SSH access.

The Accounts tab can also run the OAuth flow. It uses the server's local `127.0.0.1:5959-5968` callback ports, which works out of the box when the browser runs on the same machine. When it does not — remote or containerized deployments, or a browser that cannot reach that port — the Command Code page cannot hand the credential back automatically. In that case the page stops on a URL carrying the credential in its query string (`?apiKey=…&state=…`); paste that whole address-bar link into the **回调链接** field and submit it. The gateway parses the credential out of the link locally (it never fetches the URL), and the single-use state token still has to match the pending flow, so a forged link cannot inject an account.

### CLI OAuth

```bash
./cmdcode2api --oauth
```

The OAuth callback server always binds to `127.0.0.1:5959-5968` on the machine running the binary. A successful flow adds the account to the running process for that run only, then prints the key — this build has nowhere durable to store it, so put it in the environment to keep using it after a restart:

```text
✅ API key ready as account "main" (1 account(s) total)

This build keeps state in memory only, so the key cannot be saved.
Set it in the environment to keep using it after a restart:

  COMMANDCODE_API_KEY=<the key>
```

Run `--oauth` again (e.g. with a different browser profile) to authorize more accounts; re-authorizing a key that is already configured is a no-op.

**Browser on the same machine** — open the printed authorization URL directly.

**Browser on a different machine** — forward a callback port over SSH and pass an explicit callback URL:

```bash
# local machine
ssh -L 5959:127.0.0.1:5959 user@server

# server
./cmdcode2api --oauth --oauth-callback http://localhost:5959/callback
```

### Inside a container

The callback server listens on `127.0.0.1:5959-5968` *inside the container*, so the browser must be able to reach that port in the container's network namespace:

```bash
# browser on the same machine: share the host network
docker compose run --rm --network host cmdcode2api --oauth

# browser on a different machine: forward the port over SSH first
ssh -L 5959:127.0.0.1:5959 user@server
docker compose run --rm --network host cmdcode2api --oauth \
  --oauth-callback http://localhost:5959/callback

# without Compose
docker run --rm -it --network host \
  ghcr.io/peach0x33a/cmdcode2api:latest --oauth
```

The run prints the key as `COMMANDCODE_API_KEY=<the key>`; pass it to the gateway when you start it (as a container environment variable or a shell variable on the host) — nothing is written to a config file. A restarted server has no accounts until it gets that variable or one is added in the WebUI again.

## Command-line flags

| Flag | Meaning |
| --- | --- |
| `--oauth` | Run the browser OAuth flow to obtain a Command Code API key |
| `--oauth-callback URL` | Explicit callback URL for `--oauth`, e.g. `http://localhost:5959/callback` |
| `--host HOST` | HTTP listen host override, e.g. `0.0.0.0` |
| `--port PORT` | HTTP listen port override |
| `--debug` | Print request bodies and all upstream SSE events to stderr |
| `--version` | Print the version and Go runtime version, then exit |

## Configuration

The gateway is stateless: it reads no config file and writes none. The initial configuration is built in memory at startup, and everything else is added at runtime through the WebUI. Three optional environment variables seed the state so a redeploy keeps working without opening the WebUI first:

| Variable | Meaning |
| --- | --- |
| `CLIENT_API_KEY` | Fixed local bearer key for clients. When unset, a random `ccgw-...` key is generated on every start and printed to the logs. |
| `ADMIN_PASSWORD` | WebUI admin password. When unset, a random password is generated on every start and printed to the logs. |
| `COMMANDCODE_API_KEY` | Upstream Command Code key, seeding one account named `default` so the gateway works before you open the WebUI. |
| `GOMEMLIMIT` | Go soft memory limit (the Compose file sets `512MiB`) to keep streaming buffers bounded under concurrent load. |

Defaults that are always applied, whether or not any variable is set:

- `host` — `localhost`; use `0.0.0.0` to listen on all interfaces (the Docker image runs with `--host 0.0.0.0`).
- `port` — `11434`.
- `webui` — enabled, serving the embedded WebUI and admin API at `/webui`.
- `commandcode.base_url` — `https://api.commandcode.ai`.
- `exclude_models` — `gpt-`, `claude-`, and `gemini-`, so these prefixes are hidden from `/v1/models` and rejected by `/v1/chat/completions`. These prefixes match both plain model IDs such as `gpt-4` and provider-qualified IDs such as `openai/gpt-4` by checking the part after the final `/`. To make all models available, remove the entries in the WebUI's Models tab.

`host`, `port`, and `webui` come from the defaults or the `--host` / `--port` flags; they can also be changed in the WebUI's Settings tab, where the response reports which of them need a restart to take effect.

Accounts, client keys, and setting changes made in the WebUI apply in memory for the current process only — no restart is needed for them to take effect, but a process restart clears them.

A YAML configuration of the same shape still exists for tests and one-off import/export tooling, but the running server never reads or writes it, so editing a `config.yaml` by hand has no effect. For reference, that legacy shape is:

```yaml
api_key: ccgw-generated-local-client-key
api_keys:
  - name: default
    key: ccgw-generated-local-client-key
  - name: my-agent
    key: ccgw-another-client-key
admin_password: kR7vBn2xQm9T
webui: true
commandcode:
  base_url: https://api.commandcode.ai
  accounts:
    - name: main
      api_key: your-command-code-api-key
      enabled: true
    - name: backup
      api_key: another-command-code-api-key
host: localhost
port: 11434
exclude_models:
  - gpt-
  - claude-
  - gemini-
```

Fields:

- `api_key` — legacy single local client key. Imported into `api_keys` by the migration parser when a config is loaded by tooling; the server never loads one.
- `api_keys` — local bearer keys that clients use to call this gateway. Requests and token usage are tracked per key. Manage them in the WebUI, or seed the first one with `CLIENT_API_KEY`; entries exist in memory only and are lost on restart.
- `admin_password` — password for the WebUI admin API. Taken from the `ADMIN_PASSWORD` environment variable, or generated at startup and printed once when that is unset.
- `webui` — set to `false` to disable serving the embedded WebUI and admin API entirely.
- `commandcode.accounts` — list of Command Code credentials. Requests rotate across enabled accounts (see below). Seed one with `COMMANDCODE_API_KEY`, or add them in the WebUI; the legacy single-key field `commandcode.api_key` is still accepted by the migration parser.
- `commandcode.base_url` — Command Code API base URL.
- `host` — HTTP listen host. Defaults to `localhost`; use `0.0.0.0` to listen on all interfaces.
- `port` — HTTP listen port. Defaults to `11434`.
- `exclude_models` — model ID prefixes hidden from `/v1/models` and rejected by `/v1/chat/completions`. Maintained from the WebUI's Models tab, where the upstream catalog is shown with checkboxes; changes apply live.

## Multi-account rotation

Every chat request is sent with the next enabled account in round-robin order. When an account fails with `401`, `403`, `429`, or a 5xx, the request is retried with the next account automatically:

- A `429` puts the account into cooldown for the upstream `Retry-After` duration (60 seconds by default); cooldown accounts are skipped until they recover. If every enabled account is cooling down, the client receives `429 rate_limit_error` with the earliest recovery time.
- `400`/`422` (bad request) and client-canceled contexts are not retried.
- Failover happens before any bytes are sent to the client; once a stream has started it is never replayed on another account.
- Per-account request/token counters are kept in memory and reset on restart; error state, last error, and cooldown windows are runtime-only and visible in the WebUI.

## Client keys

`api_keys` holds the bearer keys clients use to call this gateway; different clients can each use their own key:

- Create keys in the WebUI's **Keys** tab — values are always server-generated (`ccgw-` prefix) and never accepted from input; keys can be enabled/disabled, copied, and deleted.
- Each key gets independent request and token counters, kept in memory under `client_keys` and visible at `/usage` until the process restarts.
- Keys are masked in the list; reveal or copy them on demand — the full value is shown once at creation and available via the reveal endpoint.
- The legacy single `api_key` field is still accepted by the migration parser and becomes one key named `default`.

## WebUI

With `webui` enabled (the default), the binary serves an embedded single-file admin interface under `/webui` (the root path stays free for the API):

```text
http://localhost:11434/webui
```

Log in with the server address and `admin_password`. The same `internal/web/index.html` can also be opened directly in a browser and pointed at any running instance.

Tabs:

- **Overview** — version, uptime, listen address, usage counters, account/key/model summaries, and a quota sync summary (synced / exceeded / low-balance accounts, last refresh)
- **Accounts** — add (paste a key, or run OAuth and paste the redirect link when the browser cannot reach the server), edit name/key, enable/disable, connectivity test, quota refresh, delete; per-account requests, tokens, errors, cooldown state, last error, and quota. OAuth-added accounts are named after the Command Code user automatically
- **Models** — checkbox list of upstream models; checked = exposed via `/v1/models` and callable, unchecked = hidden. This is the editor for `exclude_models` and applies live
- **Keys** — create local client API keys, enable/disable, copy, delete; per-key usage (see [Client keys](#client-keys))
- **Settings** — edit `base_url` (live), `host`/`port`/`webui` (in memory, applied on restart), and change the admin password (requires the current password; every existing admin session is kicked afterwards)
- **Logs** — tail of the in-memory log ring (last 500 lines)

Accounts and settings are added and changed only at runtime through the WebUI. Changes apply in memory for the current process only — no restart is needed for them to take effect, but they are gone when the process restarts.

Security notes: admin authentication is rate limited per source IP (5 failed attempts in 10 minutes locks the source out for 15 minutes), responses carry hardening headers (CSP, `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: no-referrer`) and are never cached, and the login form supports password managers (Bitwarden et al.). "Remember password" keeps the credential in `localStorage`; unchecked, it lives in `sessionStorage` and dies with the tab.

### Quota dashboard

The Accounts tab shows each account's Command Code quota, read with the same API key from the undocumented `/alpha/*` endpoints (`whoami`, `billing/credits`, `billing/subscriptions`, `usage/summary`):

- **5-hour** and **weekly** bars come straight from the upstream `windowLimits` objects. Bars grade amber at ≥50% used, heavier amber at ≥75%, and red at ≥90%.
- **Monthly** is *derived*: the API exposes no monthly window, so the cap comes from the community CLI's plan mapping (`individual-pro` → 30, `individual-pro-v1` → 80, …), with `used = cap − remaining credits`. It is labelled as estimated; unknown plans fall back to balance-only display.
- Credit balances (monthly remaining / purchased / free), plan name and status, billing-period end, and billing-period totals.
- A per-account **refresh quota** button and a **refresh all** button (which returns immediately and refreshes in the background).

Quota refresh runs once shortly after startup and every 5 minutes afterwards; the latest snapshot is cached in memory only, so it is lost on restart and re-fetched shortly after the next start. A failed query keeps the last successful snapshot and only records the error and check time. These endpoints come from [commandcode-usage](https://github.com/MAXeaglet/commandcode-usage); they are unofficial, so the parser tolerates field drift (camelCase or snake_case, epoch seconds / milliseconds / ISO timestamps, flat or `data`-wrapped responses).

### Admin API

The UI talks to a JSON API under `/admin/api/*`, authenticated with `Authorization: Bearer <admin_password>` (works for scripts and curl too):

```text
GET    /admin/api/overview
GET    /admin/api/accounts
POST   /admin/api/accounts             {"name": "...", "api_key": "..."}
PATCH  /admin/api/accounts/{id}        {"enabled": true}, {"name": "..."} or {"api_key": "..."}
DELETE /admin/api/accounts/{id}
POST   /admin/api/accounts/{id}/test
POST   /admin/api/accounts/{id}/quota/refresh
POST   /admin/api/quotas/refresh       202 + background refresh of every account; with {"id": "..."} it refreshes one account synchronously
GET    /admin/api/models
PUT    /admin/api/models               {"exposed": ["model-id", ...]}
GET    /admin/api/keys
POST   /admin/api/keys                 {"name": "..."} — the key value is always server-generated
GET    /admin/api/keys/{id}/reveal
PATCH  /admin/api/keys/{id}            {"enabled": true} or {"name": "..."}
DELETE /admin/api/keys/{id}
GET    /admin/api/settings
PUT    /admin/api/settings
GET    /admin/api/logs?after=SEQ
POST   /admin/api/oauth/start
POST   /admin/api/oauth/complete   {"callback_url": "<浏览器跳转后的完整链接>"}
GET    /admin/api/oauth/status
POST   /admin/api/oauth/cancel
```

`GET /admin/api/accounts` includes a nested `quota` object per account; `GET /admin/api/overview` includes a `quotas` summary (`synced`, `exceeded`, `low_balance`, `last_checked_at`).

## HTTP API

### `GET /health`

No authentication required.

```json
{"status":"ok"}
```

### `GET /usage`

No authentication required. Returns locally accumulated usage counters plus per-account and per-client-key breakdowns:

```json
{
  "total_requests": 1,
  "prompt_tokens": 7527,
  "completion_tokens": 55,
  "cache_read_tokens": 7424,
  "cache_write_tokens": 0,
  "accounts": {
    "a1b2c3d4": {"requests": 1, "prompt_tokens": 7527, "completion_tokens": 55, "cache_read_tokens": 7424, "cache_write_tokens": 0}
  },
  "client_keys": {
    "k9f8e7d6c": {"requests": 1, "prompt_tokens": 7527, "completion_tokens": 55, "cache_read_tokens": 7424, "cache_write_tokens": 0}
  }
}
```

Usage accumulates in memory and resets to zero on restart. Cached quota snapshots also live in memory but are never exposed here.

### `GET /v1/models`

Returns the model list after applying `exclude_models` filtering, so excluded models do not appear. Each entry carries a `context_window` field when the upstream reports one.

### `POST /v1/chat/completions`

Accepts OpenAI-style chat completion requests and forwards them to Command Code. Requests for excluded models return `404` with an OpenAI-compatible error JSON shape.

**Model IDs** must match `/v1/models` output exactly, including the provider prefix:

```text
deepseek/deepseek-v4-flash          ✓
deepseek-v4-flash                   ✗ missing provider prefix
deepseek-ai/deepseek-v4-flash       ✗ wrong provider prefix
```

**Supported request styles:** plain text messages, multimodal content arrays, `stream: true` server-sent events, and `stream: false` JSON responses.

**Images:** multimodal `image_url` values must be base64 `data:image/...;base64,...` URLs. Remote HTTP(S) image URLs are rejected with `400 invalid_request_error` — the gateway never downloads remote images.

Upstream `inputTokenDetails.cacheReadTokens` is exposed in the response as `usage.prompt_tokens_details.cached_tokens`. `cacheWriteTokens` is available in `/usage` but not in the Chat Completions response, because OpenAI's standard usage schema has no cache-write field.

## Running behind nginx

If nginx and cmdcode2api run on the same host, keep forwarding Cloudflare's `CF-Connecting-IP` and `X-Forwarded-For` headers. The server accepts these headers only from loopback proxy connections, then uses the resolved address for HTTP logs and admin login rate limiting. Direct connections with forged proxy headers continue to use their TCP peer address.

For `X-Forwarded-For`, only the rightmost entry (the one an appending proxy wrote) is honored: leftmost entries are client-controlled, and a client forging a fresh one per request would rotate its rate-limit key. Do not preserve the client-supplied header via `proxy_add_x_forwarded_for`, and prefer allowing only Cloudflare's published proxy CIDRs at the nginx level so `CF-Connecting-IP` cannot be forged by connecting to the origin directly. If nginx itself also needs `$remote_addr` to represent the end user, configure `real_ip_header CF-Connecting-IP` with Cloudflare's published proxy CIDRs.

Client Bearer Tokens are any key created in the WebUI's Keys tab, or the one seeded from `CLIENT_API_KEY`.

## Project layout

```text
cmd/cmdcode2api/   CLI entrypoint
internal/app/      gateway implementation
internal/web/      embedded single-file WebUI (index.html)
```

## Files intentionally not committed

The repository ignores build artifacts and leftovers from earlier, file-backed versions. The current server writes none of these at runtime:

```text
cmdcode2api
cc-gateway
config.yaml
usage.json
*.exe
.oauth_state
.oauth_url
```

## Notes

This is a personal utility gateway and currently targets the Command Code API shape observed during development. If Command Code changes its internal API, the adapter may need updates. The project was originally named `cc-gateway` and was renamed to avoid confusion with Claude Code's common `cc` abbreviation.
