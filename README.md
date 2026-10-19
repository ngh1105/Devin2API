# Devin2API
OpenAI & Anthropic compatible reverse proxy API for Devin — built from scratch in Go.

**Milestone 1:** OAuth login (Auth0 PKCE), account persistence as JSON, a web panel (accounts + tokens pages), and a token-gated `/v1` proxy stub. The chat completions endpoint currently returns **501 "upstream not implemented yet"** (as plain `data:` frames per the OpenAI stream protocol). The next milestone is implementing the Devin upstream chat protocol (WebSocket) to make `/v1/chat/completions` functional.

---

## Build

```bash
go build ./cmd/devin2api
```

Requires **Go 1.26 or newer**. Windows users get a `.exe` binary.

---

## Run

```bash
DEVIN2API_LISTEN="127.0.0.1:8080" DEVIN2API_DATA="./data" ./devin2api
```

| Env variable   | Default            | Description                                    |
|----------------|--------------------|------------------------------------------------|
| `DEVIN2API_LISTEN` | `127.0.0.1:8080` | Address to bind the HTTP server.             |
| `DEVIN2API_DATA`   | `./data`         | Root directory for accounts, tokens, config. |

The server exposes:

- **Web panel** — open <http://127.0.0.1:8080/> in a browser; jump directly to the accounts page at `/web/accounts.html`.
- **Health check** — `GET /healthz` → `{"status":"ok"}`.
- **OpenAI-compatible `/v1/*` endpoints** — see [API status](#api-status).

> **Warning about non-loopback binding.** Panel pages are unauthenticated. The admin API (`/admin/api/*`) only answers requests whose `Host` is `localhost`, `127.0.0.1` or `[::1]`, rejects cross-origin requests, and requires an `X-Devin2API` header on state-changing calls (the panel sends it). Token values are masked in every list response. Do not expose the server on a public interface without adding your own auth/gateway layer.

---

## Login

Run the headless login command to authenticate against `auth.devin.ai` and save an account:

```bash
devin2api login
```

1. You will be prompted for **Email**, then **Password**.
2. The tool performs a full Auth0 PKCE hosted-login flow (identifier → password → authorize → userinfo).
3. On success it prints something like:

```
saved data/accounts/acc_a1b2c3d4.json
```

Supported account types: email+password sign-ups on Devin. Enterprise accounts that use Google, GitHub, SAML, or other IdP logins are **not supported** via the headless flow — those routes redirect to the external identity provider, which the CLI cannot follow automatically. Users with such identities can obtain tokens manually through the browser's DevTools and import them via the [Accounts panel textarea](#account-json-import).

---

## Account JSON

### Schema (v1)

```jsonc
{
  "schema": 1,
  "id":        "acc_XXXXXXXX",    // auto-generated acc_ + 16 hex chars
  "email":     "user@example.com",
  "sub":       "auth0|xxxxxxxxx", // Auth0 sub identifier
  "created_at":"2026-10-18T...",   // ISO timestamp
  "oauth": {                       // Auth0 connection info
    "client_id": "…",
    "domain":    "auth.devin.ai",
    "audience":  "…",
    "redirect_uri": "…"
  },
  "tokens": {                      // Bearer credentials
    "access_token":  "…",          // IDP access token
    "refresh_token": "…",          // used to renew expired tokens
    "id_token":      "…",
    "expires_at":    1729251234,   // unix seconds (computed from expires_in)
    "scope":         "openid …"
  },
  "profile": {                     // userinfo payload
    "name":  "Display Name",
    "picture": "https://…"
  },
  "usage": null                    // reserved — future per-account quotas
}
```

Accounts are persisted as individual `<id>.json` files under `$DEVIN2API_DATA/accounts/`.

### Importing Accounts

You can paste JSON into the Accounts panel textarea (`/web/accounts.html`) to import accounts:

| Input shape                        | Result                                           |
|------------------------------------|--------------------------------------------------|
| Full schema blob (all v1 fields)   | Parsed as-is; same-sub re-imports upsert.       |
| A bare `"access_token": "..."` blob | New account generated; only the token is set.   |
| A bare `"refresh_token": "..."` blob| New account generated; only the refresh token.  |

Re-importing an account updates the existing record instead of creating a duplicate. Accounts match by `sub`, or by token value for bare token blobs. The original `id` and `created_at` are kept. Imported `id` values may only contain letters, digits, `_` and `-`. An account holding only a refresh token shows as expired until you refresh it.

---

## Downstream Tokens (Proxy Authentication)

Use the **Tokens** page (`/web/tokens.html`) in the panel to create tokens. Each call generates:

- **ID** — internal `t_XXXXXXXX`
- **Value** — `d2sk_` + 32 hex characters (shown once on creation; later listings show it masked, so store it securely)

Include the token value with any request to the `/v1/*` endpoints:

```
Authorization: Bearer d2sk_xxxx...
```

or equivalently:

```
X-Api-Key: d2sk_xxxx...
```

If the token store is **empty** (no tokens created), the proxy accepts requests without authentication. As soon as one or more tokens exist, every `/v1/*` request must include a valid token.

---

## API Status

| Endpoint                             | Method   | Current behavior                                      |
|--------------------------------------|----------|-------------------------------------------------------|
| `GET /healthz`                       | GET      | Returns `{"status":"ok"}`.                            |
| `GET /v1/models`                     | GET      | Returns the static model list (model id: `devin`).    |
| `POST /v1/chat/completions`          | POST     | **501** — body contains `{"error":{"message":"upstream not implemented yet","type":"devin2api_error"}}`.<br>When `stream: true`, the error arrives as SSE `data:` frames. |

The `/v1/chat/completions` endpoint mirrors the OpenAI Chat Completions wire format (request shape accepted as-is), but delegates upstream calls to Devin's actual chat protocol — which has not been implemented yet. This is **the next milestone**.

---

## Roadmap

| Milestone | What                          | Status         |
|-----------|-------------------------------|----------------|
| 1         | OAuth login + account JSON + web panel + `/v1` proxy stub | Done           |
| 2         | Devin upstream chat protocol (WebSocket) — brings `/v1/chat/completions` live | Next           |

Once milestone 2 lands, the chat completions endpoint will forward user messages to Devin over WebSocket, stream back responses, and complete the OpenAI-compatible experience.

---

## Project Structure

```
cmd/devin2api/          # CLI entry point (login + serve subcommands)
internal/auth/          # Auth0 PKCE login client
internal/config/        # Environment-variable config loader
internal/server/        # HTTP handlers (/v1/*, web panel, admin API, healthz)
internal/store/         # Account and token file-based stores
web/                    # Static HTML/CSS/JS for the web panel
```
