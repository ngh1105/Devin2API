<p align="center"><img src="assets/logo.svg" width="112" alt="Devin2API logo"></p>

# Devin2API

OpenAI-compatible reverse proxy API for Devin — built from scratch in Go.

**Milestones achieved:**
1. OAuth login (browser-link, any Devin auth method incl. Google/SSO)
2. Web panel (accounts, quota display, API keys, settings), CLI login, live usage/quota panel, CLIProxyAPI-compatible account JSON import, token-gated `/v1`
3. Live `/v1/chat/completions` (stream + non-stream) backed by Devin's `GetChatMessage` API, with account rotation and failover

**What it does:** Serves OpenAI-compatible `/v1/*` endpoints backed by your Devin accounts.

---

## Install

**Prebuilt binaries**: each tagged release on GitHub has archives for Windows, Linux and macOS (amd64 and arm64) plus `checksums.txt`. `devin2api version` prints the version.

**Docker**: release images are published to `ghcr.io/ngh1105/devin2api`.

```bash
docker run -d --name devin2api -p 127.0.0.1:8080:8080 -v devin2api-data:/data \
  -e DEVIN2API_ADMIN_PASSWORD='choose-a-password' ghcr.io/ngh1105/devin2api:latest
```

- Inside the container the server listens on `0.0.0.0:8080` and keeps data in the `/data` volume.
- **Set `DEVIN2API_ADMIN_PASSWORD`.** Requests reach the container over Docker's bridge network, not loopback, so without a password the panel and admin API refuse every request. For the same reason, `/v1` needs an API key: create one in the panel.
- Publishing on `127.0.0.1` (as shown) keeps it on this machine. To use it from the LAN, publish on all interfaces (`-p 8080:8080`).
- The browser login waits for a callback on a port inside the container, so it cannot finish there. Log in with the binary on the host and import the account JSON in the panel, or use the manual token login.

**From source**:

```bash
go build ./cmd/devin2api
```

Requires **Go 1.26.9 or newer** (an older 1.26 toolchain fetches it automatically). Windows users get a `.exe` binary. To build an image locally: `docker build --build-arg VERSION=dev -t devin2api .`

**Releasing** (maintainers): push a tag such as `v0.1.0`. The Release workflow tests, builds the archives, creates the GitHub release and pushes the multi-arch image. Running the workflow by hand is a dry run: it only uploads the archives as a workflow artifact.

---

## Run

```bash
DEVIN2API_LISTEN="127.0.0.1:8080" DEVIN2API_DATA="./data" ./devin2api
```

| Env variable   | Default            | Description                                    |
|----------------|--------------------|------------------------------------------------|
| `DEVIN2API_LISTEN` | `127.0.0.1:8080` | Address to bind the HTTP server.             |
| `DEVIN2API_DATA`   | `./data`         | Root directory for accounts, tokens, config. |
| `DEVIN2API_ADMIN_PASSWORD` | (unset) | Protects the panel and admin API with a login, and allows using them from other hosts (e.g. the LAN). |
| `DEVIN2API_RESPONSES_KEEP` | `1024` | How many `/v1/responses` histories to keep for `previous_response_id`. |
| `DEVIN2API_RESPONSES_TTL` | `24h` | How long to keep them (a Go duration such as `2h` or `30m`). |
| `DEVIN2API_RESPONSES_MB` | `64` | Total size budget of the kept histories, in MB. |

The server exposes:

- **Web panel** — open <http://127.0.0.1:8080/> in a browser; single-page app at `/web/` with tabs (Accounts, API Keys, Logs, Settings, API).
- **Health check** — `GET /healthz` → `{"status":"ok"}`.
- **OpenAI-compatible `/v1/*` endpoints** — see [API status](#api-status).

> **Security.** The admin API (`/admin/api/*`) always rejects cross-origin requests and requires an `X-Devin2API` header on state-changing calls (the panel sends it). Session tokens and API keys are masked in every admin response. Imported account `id` values may only contain letters, digits, `_` and `-`.
>
> - **Without `DEVIN2API_ADMIN_PASSWORD`** (the default) the panel needs no login. The admin API and keyless `/v1` then only serve local clients: the connection must come from a loopback address, and the `Host` must be `localhost`, `127.0.0.1` or `[::1]`. That covers both direct network requests that forge the `Host` header and DNS rebinding from a browser.
> - **With `DEVIN2API_ADMIN_PASSWORD`** the panel asks for the password.
>   - A login lasts 7 days, in an `HttpOnly`, `SameSite=Strict` cookie. Sessions are in memory, so a restart signs everyone out.
>   - Failed logins are slowed down and capped at 10 per minute.
>   - The admin API is then served on any host, so you can bind `0.0.0.0` and use the panel from the LAN.
>   - Keyless `/v1` access stays loopback-only, so create API keys for other machines.
>   - Traffic is plain HTTP: put a TLS reverse proxy in front before exposing it beyond a trusted network.
> - **Behind a reverse proxy** (even one on the same machine) always set `DEVIN2API_ADMIN_PASSWORD` and create API keys: the proxy's connections come from a loopback address, and one that rewrites `Host` to `127.0.0.1` (nginx's default) would make every request look local. Pass the client's `Host` through (nginx: `proxy_set_header Host $host`), since admin writes are refused when the browser's `Origin` does not match it.
> - The response cache (on by default, see [Panel](#panel)) keeps model answers in `<data>/cache.json`; turn it off if outputs must not be written to disk.

---

## Login

### Panel — browser login (preferred, any IdP)

1. Open the panel at <http://127.0.0.1:8080/web/>, switch to the **Accounts** tab.
2. Click **Add account**, then **Login with browser**.
3. A link to `auth.devin.ai` opens in your default browser. Log in with **any** Devin auth method — email+password, Google, GitHub, SAML, etc.
4. After successful auth you're redirected back to the local server; the account appears with usage data.

### Panel — Manual token import

Paste a Devin session token or full JSON blob into the **Import** card on the Accounts tab:

- Full token string: `devin-session-token$eyJ...`
- Raw JWT: auto-prefixed to `devin-session-token$<jwt>` on import.
- Shape: `{"token": "<value>"}` or full v2 schema JSON.

On success the account appears with usage data.

Alternatively via the API endpoint:

```bash
curl -X POST http://localhost:8080/admin/api/login/manual \
  -H "Content-Type: application/json" \
  -d '{"token":"devin-session-token$eyJ..."}'
```

### Dead-token handling

Server-side session tokens expire (TTL'd). If a token is dead:

- Usage queries surface a **"Token dead"** badge alongside the account name.
- The account remains; re-login via the steps above to obtain a fresh token.

---

## Account JSON

Accounts are persisted as individual `<id>.json` files under `$DEVIN2API_DATA/accounts/`. The store accepts three import shapes:

### Schema v2 (full import)

```jsonc
{
  "schema": 2,
  "id":          "acc_XXXXXXXX",    // optional — autogenerated if absent
  "email":       "user@example.com",
  "user_name":   "handle",
  "user_id":     "user-xxxx",
  "org_id":      "org-xxxxx",
  "plan":        "Pro",
  "nickname":    "",                // optional human label
  "enabled":     true,
  "auth_kind":   "oauth",
  "created_at":  "2026-10-18T...",  // ISO timestamp
  "tokens": {
    "session_token": "devin-session-token$eyJ..."
  },
  "usage": null                     // populated after first usage fetch
}
```

### CLIProxyAPI import (recommended)

Drop in a JSON blob exported from a client like curl-curl-in-js or Postman. Both `api_key` and `session_token` fields are accepted; if both are present, `session_token` takes precedence. Raw JWT values are auto-prefixed with `devin-session-token$`.

**Exact example (real file):**

```json
{"api_key":"devin-session-token$eyJ...","auth_kind":"oauth","disabled":false,"email":"user@hotmail.com","org_id":"org-...","plan":"Pro","session_token":"devin-session-token$eyJ...","type":"devin","user_id":"user-...","user_name":"handle"}
```

Fields extracted: email, user_name, user_id, org_id, plan, auth_kind, enabled. Tokens from `session_token` or `api_key` become `tokens.session_token`.

### Bare token blob

```json
{"access_token": "raw-jwt-here"}
```

Only token-bearing keys (`access_token`, `refresh_token`, `token`) plus optional identity fields (`user_id`, `email`, `user_name`, `org_id`, `api_key`, `session_token`) are accepted. Generates a minimal account with schema v2.

Same-user re-import (by `user_id` or `email`) merges the new data into the existing record — accounts stay deduplicated.

---

## Usage

Usage data comes from Devin's seat-management API:

- **Plan** — e.g. "Pro".
- **Daily / Weekly remaining** — percentage bars (`daily_remaining_percent`, `weekly_remaining_percent`).
- **Reset times** — `daily_reset_at` and `weekly_reset_at` ISO timestamps.
- **Plan window** — `plan_start` and `plan_end`.

Refresh per account (Accounts tab → Refresh) or refresh all. Errors surface as a "Token dead" badge or an error message under the account row.

---

## Panel

Single-page app at `/web/` with five tabs:

| Tab          | Contents                                                        |
|--------------|-----------------------------------------------------------------|
| **Accounts** | Account list with usage bars, search, status filter and pages of 10–100; add (browser login, pasted session token or JSON import), enable/disable, delete, refresh usage. Tick rows (or the header box for the page, then "Select all matching") to refresh, enable, disable or delete many at once. |
| **API Keys** | Create keys with a name, expiry date, model allow list and request quota; edit, disable, reset the counter or revoke them. Shows each key's status, requests used and last use. |
| **Logs**     | A chart of the last 24 hours (succeeded / failed requests per hour, tokens in and out), the top models, accounts and API keys, and the last 1000 chat requests with search, filters, pages and auto-refresh. Click a row for every detail: endpoint, models, account, API key, attempts, status, duration, time to first output, tokens, tool calls, finish reason and upstream error. |
| **Settings** | Devin endpoints (https only, except localhost), global system prompt, response cache (on/off, retention, size), and the listen address / data directory (read-only, set via environment variables) |
| **API**      | Endpoint reference and a curl example                           |

The panel follows the OS light/dark setting; the Light / System / Dark switch in the sidebar overrides it for that browser. Destructive actions (deleting accounts, revoking keys, clearing the log) ask for confirmation first.

Settings saved from the panel apply immediately and are kept in `<data>/settings.json`:

- **Devin endpoints**: an ordered list of Devin server base URLs (default `https://server.codeium.com`). Chat, usage and model requests use the first one. On a network error or HTTP 502/503/504 before any output, the same account is retried on the next endpoint; other errors (a rejected token, a rate limit, a bad request) are not endpoint problems and go to account failover as before. **Test** checks that a URL answers without sending any account token. Account session tokens are sent to every listed endpoint, so only add hosts you trust.
- **Global system prompt**: when switched on, added before (default) or after the client's own system prompt on every chat endpoint (`/v1/chat/completions`, `/v1/messages`, `/v1/responses`, `/v1/completions`). It goes through the same cleaning as client prompts. Up to 32 KB.
- **Response cache** (on by default): a chat request identical to an earlier successful one is answered from memory and spends no account usage. The key covers everything that shapes the answer: the resolved model, the system prompt (with the global one applied), every message including images and tool results, the tool definitions, `max_tokens`, `temperature`, `top_p` and stop sequences; `cache_control` markers are ignored, so a request with and without them is the same entry. A hit replays the recorded output, so it works for every endpoint, streamed or not. Unlike a provider's prompt cache, which only speeds up the prompt and still samples a fresh answer, this replays the earlier answer: an identical request within the window gets the same text even at `temperature` 1, so turn the cache off if clients resend a prompt to get a different sample ("regenerate", best-of-N). The cache is shared by all API keys. It borrows the Anthropic prompt cache conventions for lifetime and usage: an answer lives for 5 minutes after its last use (every hit extends it, like the ephemeral cache; set 60 for the 1-hour behaviour), and the hit's `usage` reports the prompt as read from the cache: on `/v1/messages`, `input_tokens` is 0 and `cache_read_input_tokens` holds the count; on the OpenAI endpoints `prompt_tokens` is unchanged and `prompt_tokens_details.cached_tokens` (`input_tokens_details.cached_tokens` on `/v1/responses`) holds it, so clients and cost trackers see what was not spent. Up to 1000 answers and 64 MB are kept (least recently used evicted first; lowering a limit also trims the saved file on the next start); all three limits are editable, and `0` or an empty field means the default. The cache is saved to `<data>/cache.json` (the full answers, in plain text) within ~2 s of a new answer and within a minute of a hit, and survives restarts. **Clear cache** (or `DELETE /admin/api/cache`) drops it; `GET /admin/api/cache` reports its size and the totals. Cached requests still count against an API key's request quota, since the key did make a request. Failed requests, empty answers and refusals (`content_filter`) are never cached, so a retry reaches upstream. Devin has no prompt cache the proxy can use, so a request that merely shares a prefix with an earlier one (the usual agent turn) still costs its full prompt upstream; only whole repeated requests are saved.

Stats grid at the top: Accounts, Models, Requests (with a 24-hour sparkline), Failed, Cache hits (share of all requests, with hits and tokens saved). Request, failure and cache-hit totals (with the tokens the hits saved), and per-hour counts for the last 7 days, are saved to `<data>/stats.json` (within ~2 s of each request) and survive restarts; the date counting started is shown under them. Delete the file to reset. In the hourly counts, a cached request's tokens go to `saved_input_tokens` / `saved_output_tokens` instead of `input_tokens` / `output_tokens`, so the token totals show what was spent upstream.

The request log keeps the last 1000 chat requests in memory and appends each one to `<data>/logs.jsonl`, so it survives restarts; the file is trimmed back to 1000 lines once it reaches 2000. **Clear** in the Logs tab (or `DELETE /admin/api/logs`) empties it.

On Ctrl+C or SIGTERM the server stops accepting connections, gives open requests up to 5 s, then cancels any streams still running (they are logged as failed) and waits up to 2 s more before writing the counters it otherwise saves with a short delay. Files under the data directory are written through a temporary file, fsynced and renamed, so a crash leaves the old or the new version, never a truncated one.

A chat stream that sends no frame for 3 minutes (or no response headers within 60 s) is abandoned with a 504 and the next endpoint or account is tried, instead of hanging until the client gives up. Each account sends its own device fingerprint upstream.

Every chat request is also logged to the console (stderr) as one line, for example:

```
INFO chat endpoint=/v1/messages model=claude-sonnet-4-6 stream=true account=acc_... attempts=1 status=200 ms=972 first_ms=889 in=418 out=5 finish=stop
INFO chat endpoint=/v1/messages model=claude-sonnet-4-6 stream=true account="" attempts=0 status=200 ms=1 first_ms=0 in=418 out=5 cached=true finish=stop
```

The second line is a cache hit: no account was tried and the tokens shown were saved, not spent. In the panel's log such a request shows **cache** in the account column.

Logs hold metadata only; prompts, outputs and credentials are never recorded. (The response cache, when on, does keep answers in `cache.json`; see above.)

---

## Downstream Tokens (Proxy Authentication)

Use the **API Keys** tab in the panel to create keys. Each key has:

- **ID** — internal `t_XXXXXXXX`
- **Value** — `d2sk_` + 32 hex characters, shown once on creation. Only its SHA-256 hash and a masked hint (`d2sk_…ab12`) are stored in `<data>/tokens/tokens.json`, so copy it when it is shown. Keys saved in plaintext by older versions are hashed on the first start and keep working.
- **Name** (optional) — shown in the panel and in the request log.
- **Expiry** (optional) — after the chosen day the key gets 401 `API key has expired`.
- **Models** (optional) — an allow list; other models get 403, and `/v1/models` lists only the allowed ones.
- **Request quota** (optional) — the number of generation requests (`POST`s other than `count_tokens`) the key may make in total; past it the key gets 429. The panel shows the count and can reset it.
- **Disabled** — refuses every request (401) without deleting the key.

Keys are managed with `GET/POST /admin/api/tokens` and `PUT/DELETE /admin/api/tokens/{id}`; `PUT` takes `{"name", "disabled", "expires_at", "models", "quota", "reset_requests"}`.

Include the token value with any request to the `/v1/*` endpoints:

```
Authorization: Bearer d2sk_xxxx...
```

or equivalently:

```
X-Api-Key: d2sk_xxxx...
```

If the token store is **empty** (no tokens created), `/v1/*` accepts requests without a key, but only from local clients: the connection must come from a loopback address, the `Host` must be `localhost`, `127.0.0.1` or `[::1]`, and any `Origin` header must match it. This stops web pages and DNS-rebinding tricks from spending your Devin quota. As soon as one or more tokens exist, every `/v1/*` request must include a valid token. Key values are never returned after creation; lists show the masked hint.

---

## API Status

| Endpoint                             | Method   | Current behavior                                      |
|--------------------------------------|----------|-------------------------------------------------------|
| `GET /healthz`                       | GET      | Returns `{"status":"ok"}`.                            |
| `GET /v1/models`                     | GET      | Lists the live model catalog from Devin (`GetCliModelConfigs`, cached 30 min) with display name, context length and multimodal flag. `devin` is an alias for `swe-2-high`. |
| `POST /v1/chat/completions`          | POST     | Chat completion through Devin. Supports `stream: true` (SSE). |
| `POST /v1/messages`                  | POST     | Anthropic Messages API (Claude Code, Anthropic SDKs). Stream + non-stream. |
| `POST /v1/messages/count_tokens`     | POST     | Rough estimate (~4 bytes per token); upstream has no counting endpoint. |
| `POST /v1/responses`                 | POST     | OpenAI Responses API. Stream + non-stream; `previous_response_id` supported (kept in memory, per API key). |
| `POST /v1/completions`               | POST     | Legacy text completions, emulated on the chat models. Stream + non-stream. |
| `POST /v1/embeddings`                | POST     | Not supported: returns 501. Devin offers no embedding models. |

How chat works:

- Each request goes to `server.codeium.com/exa.api_server_pb.ApiServerService/GetChatMessage` (Connect + protobuf) using the session token of an enabled account.
- Account choice per request:
  - Accounts with more quota left are tried first, using the tighter of the daily and weekly remaining percent in 10% bands. Accounts in the same band take turns (round-robin).
  - Usage is refreshed in the background every 10 minutes (first run ~5 s after start), so this ranking stays current without pressing Refresh.
  - After a 429, the account is parked for that model until the reset Devin states ("reset in 21 minutes"), or 1 minute if it states none. Free-model rate limits apply per model, so the account keeps serving other models.
  - Accounts that are parked, out of daily or weekly quota, or whose token was rejected go last. They are still tried, so a request only fails when every account fails.
- If an account's token is rejected (401) or it hits a limit (429/403) before any output is sent, the next account is tried. A rejected token is flagged in the panel so you can re-login.
- `model` accepts the ids from `/v1/models`. A `devin/` prefix is dropped and dots become dashes (`claude-sonnet-4.6` → `claude-sonnet-4-6`). A family name without an effort level picks one from the catalog, trying medium, then high, then low (`gpt-5-4` → `gpt-5-4-medium`).
- `system`/`developer` messages become the system prompt; `user` and `assistant` messages become the conversation. `max_completion_tokens` (or the older `max_tokens`; default 32000), `temperature` and `top_p` are forwarded. Streams include usage only when `stream_options.include_usage` is set, in a final chunk with empty `choices`.
- `stop` (a string or up to 4 strings) is applied by the proxy, since upstream has no stop option: the output is cut before the first match and the upstream stream is closed. `finish_reason` is `"stop"`. Only answer text is checked, not thinking or tool arguments. Upstream sends token usage only at the end of its stream, so after a stop the usage is estimated (about 4 bytes per token).
- System prompts are cleaned before going upstream: Devin rejects prompts that carry another agent's identity (e.g. "You are Claude Code, ...") with a misleading 403, so such lines, Claude Code billing headers and a few related lines are dropped. This applies to all three chat endpoints. Each drop is logged (line count and the start of the first line), so a prompt of your own that matches one of the rules does not lose text unnoticed.
- Thinking text, when the model produces it, is returned as `reasoning_content`.
- Tool calling follows the OpenAI format: `tools` (function type) are forwarded with their JSON schema; the model's calls come back as `tool_calls` (streamed as indexed fragments) with `finish_reason: "tool_calls"`; send results back as `role: "tool"` messages with `tool_call_id`.
- `tool_choice`: `"none"` drops the tools; `"auto"` is the default. Upstream cannot force a tool call, so `"required"` and a named function are best effort: a named choice narrows the tools to that one, and both add an instruction to the system prompt telling the model to call a tool. Models follow it reliably in testing, but it is not guaranteed.
- Images: user messages may include `image_url` parts with base64 data URLs (`data:image/png;base64,...`; png, jpeg, webp, gif). Remote image URLs are not fetched and return 400, as do images sent to a model without `"multimodal": true` in `/v1/models`. Request bodies on `/v1` may be up to 32 MB.
- Rate limits: when Devin returns 429 with a stated wait ("reset in 22 minutes"), the response carries a matching `Retry-After` header (seconds).
- Errors use the OpenAI error shape with a matching `type` (`invalid_request_error`, `rate_limit_error`, `server_error`, ...). With no usable account the endpoint returns 503. When Devin rejects the proxy's own account tokens (upstream 401/403), clients get 502 rather than 401, so they do not blame their API key; re-login the account in the panel.

`/v1/messages` (Anthropic format):

- `system` (string or text blocks), `messages` with `text`, `image` (base64 source only), `tool_use` and `tool_result` blocks, `tools` with `input_schema`, `max_tokens`, `temperature`, `top_p`. `tool_result` with `is_error: true` is sent as "Error: ...". `top_k` is ignored.
- `stop_sequences` are applied by the proxy as for `stop` above; a match ends the message with `stop_reason: "stop_sequence"` and the matched `stop_sequence`.
- Server tools (`web_search_*`, `code_execution_*`, ...) are skipped, since they run on Anthropic's side. `tool_choice` `none`, `auto`, `any` and `tool` map to the OpenAI forms above (`any` = required, `tool` = named, both best effort).
- Thinking blocks are returned only when the request sets `thinking` (`enabled` or `adaptive`). Their `signature` is empty because upstream provides none. Thinking blocks in the history are dropped.
- Streaming uses the standard events: `message_start`, `content_block_*`, `message_delta` (stop reason and usage), `message_stop`. Errors use the Anthropic error shape.
- Point Claude Code at it with `ANTHROPIC_BASE_URL=http://127.0.0.1:8080` and `ANTHROPIC_API_KEY` set to a panel API key (or any value in keyless mode). `ANTHROPIC_MODEL` takes any id from `/v1/models`.

`/v1/responses` (OpenAI Responses format):

- `input` as a string or items: `message` (roles user/assistant/system/developer, `input_text`/`output_text`/`input_image` with a data URL), `function_call`, `function_call_output`. `reasoning` items are ignored. `instructions` becomes the system prompt.
- Function tools are forwarded; built-in tools (`web_search`, `file_search`, ...) are skipped. `tool_choice` works as for chat completions (named form `{"type":"function","name":...}`). `max_output_tokens`, `temperature` and `top_p` are forwarded.
- Output items: `reasoning` (summary text), `message` (`output_text`), `function_call`. Streaming emits the standard `response.*` events with `sequence_number`.
- `previous_response_id`: responses are kept in memory for 24 hours (up to 1024 and 64 MB in total, oldest dropped first; see `DEVIN2API_RESPONSES_*`), so a follow-up can send just the new input. Each response belongs to the API key that made it, so another key cannot continue it. They are lost on restart; an unknown id returns 400. `store: false` skips keeping a response. Instructions are not carried over, as in the OpenAI API.

`/v1/completions` (legacy OpenAI text completions):

- Upstream has only chat models, so the `prompt` is sent as a user message with a system prompt asking the model to output only the continuation. Results are close to a base model's, not identical: the model may still answer instead of continuing.
- `prompt` is a string or a list holding one string; token arrays and batched prompts return 400. `max_tokens` (default 32000), `temperature`, `top_p`, `stop`, `echo` and `stream_options.include_usage` work as above. `suffix`, `n` > 1, `best_of` > 1 and `logprobs` return 400; `logprobs` in the response is always null.
- Reasoning models spend part of `max_tokens` on thinking, which this format cannot return, so a small limit (e.g. 30) can end with `finish_reason: "length"` and no text.

---

## Roadmap

| Milestone | What                                                      | Status   |
|-----------|-----------------------------------------------------------|----------|
| 1         | OAuth login (Auth0 PKCE), account persistence, web panel, `/v1` proxy stub | Done     |
| 2         | Browser-link login (any IdP), usage/quota panel, CLI login, CLIProxyAPI JSON import, token-gated /v1 stub | Done |
| 3         | Devin chat upstream (`GetChatMessage`) — `/v1/chat/completions` live, account failover | Done |
| 4         | Live model list, tool calls, images, Retry-After | Done |
| 5         | `/v1/messages` (Anthropic) and `/v1/responses` (OpenAI Responses) | Done |

---

## Project Structure

```
cmd/devin2api/          # CLI entry point (login + serve subcommands)
internal/devin/         # OAuth client, session tokens, seat-management API
internal/config/        # Environment-variable config loader
internal/server/        # HTTP handlers (/v1/*, web panel, admin API, healthz)
internal/store/         # Account and token file-based stores
web/                    # Static HTML/CSS/JS for the web panel
```
