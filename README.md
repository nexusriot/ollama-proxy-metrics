# Ollama Proxy + Metrics

A lightweight **Go reverse proxy** for Ollama that adds:

- **Per-request SQLite persistence** — tokens, latency, TTFT, bytes, cost, session, error
- **Prompt & response capture** — the actual prompt and assistant text per request
- **OpenAI-compatible** — proxies and parses both Ollama's `/api/*` and the `/v1/*` (OpenAI) API
- **Cost estimation** — per-model `$/1K-token` pricing → cost per request, day, session, model
- **TTFT & throughput** — time-to-first-token and tokens/sec for streaming requests
- **Structured JSON logging** to stdout and a size-rotated log file
- **Prometheus `/metrics`** endpoint + a ready-to-import **Grafana dashboard**
- **REST API** for the dashboard (`/admin/api/*`) with **live tail (SSE)** and **CSV/JSON export**
- **React dashboard** — per-request, per-day, per-session, and **per-model** views
- **Model-aware routing** across multiple upstreams, with 5xx retry and a circuit breaker
- **Response cache**, **concurrency queue**, and per-session **rate limits** and **daily budgets**
- **Graceful shutdown** — in-flight generations finish on `SIGTERM`

## Architecture

```
┌──────────────┐  /api/* /v1/*  ┌──────────────────────┐  upstream(s)  ┌────────────────┐
│  Client App  │ ────────────▶  │  ollama-proxy (Go)   │ ────────────▶ │  Ollama :11434 │
│  (curl / SDK)│                │  :8080               │  model-aware  │  external OR   │
└──────────────┘                │                      │  + failover   │  bundled (opt) │
                                │  /metrics  (Prom)    │               └────────────────┘
                                │  /healthz /readyz    │
                                │  /admin/api/* (REST) │
                                │  SQLite + log file   │
                                └──────────────────────┘
                                          ▲
                       ┌──────────────────┼───────────────────┐
              ┌────────┴─────────┐                    ┌────────┴─────────┐
              │  frontend (nginx)│                    │  Grafana (opt)   │
              │  :3000           │                    │  :3001           │
              └──────────────────┘                    └──────────────────┘
```

The proxy forwards `/api/*` (Ollama native) and `/v1/*` (OpenAI-compatible) to
whatever `OLLAMA_UPSTREAM` points at — one URL, or several comma-separated for
model-aware routing with failover. By default that is an Ollama you run yourself; the
compose file also ships a **commented-out `ollama` service** you can opt into.

## Ports at a glance

| Port  | Service    | Exposed by       | What it serves                                     |
|-------|------------|------------------|----------------------------------------------------|
| 8080  | proxy      | host ↔ container | Reverse proxy (`/api/*`, `/v1/*`), `/metrics`, `/healthz`, `/readyz`, REST API (`/admin/api/*`) |
| 3000  | frontend   | host ↔ container | React metrics dashboard (nginx)                    |
| 11434 | ollama     | host ↔ container | Ollama HTTP API — **external by default**; bundled service is commented out in `docker-compose.yml` (opt-in) |
| 9090  | prometheus | host ↔ container | Prometheus UI — opt-in (`--profile monitoring`)    |
| 3001  | grafana    | host ↔ container | Grafana w/ pre-provisioned dashboard — opt-in (`--profile monitoring`) |

> **Rule of thumb:** point all your Ollama clients at **`:8080`** instead of `:11434`.
> The proxy is 100% transparent — every request is forwarded unchanged.

## Quick Start

### Docker Compose (recommended)

```bash
git clone https://github.com/nexusriot/ollama-proxy-metrics
cd ollama-proxy-metrics

# Tell the proxy where your Ollama lives (see "Connecting Ollama" below).
cp .env.example .env
# edit .env → set OLLAMA_UPSTREAM

docker compose up --build
```

> The default upstream is `http://ollama:11434`, which only resolves if you
> enable the bundled Ollama service (Scenario 3). If you already run Ollama on
> your host or another machine, set `OLLAMA_UPSTREAM` accordingly first.

Data is written to `./data/` in the project directory (bind mount):

```
ollama-proxy-metrics/
└── data/
    ├── db.sqlite          ← SQLite metrics database
    └── logs/
        └── proxy.log      ← structured JSON request log
```

**Prometheus** is opt-in to keep the default stack light:

```bash
docker compose --profile monitoring up --build
```

### Connecting Ollama

The proxy never starts Ollama for you unless you opt in (Scenario 3). Point it at
your Ollama via `OLLAMA_UPSTREAM` (in `.env` or directly in `docker-compose.yml`).

#### Scenario 1 — Ollama already running on your host (most common)

If Ollama is running on your machine (`localhost:11434`), point the proxy at the
host from inside the container:

```bash
# .env
OLLAMA_UPSTREAM=http://host.docker.internal:11434   # Mac / Windows
# OLLAMA_UPSTREAM=http://172.17.0.1:11434           # Linux (docker bridge gateway)
```

```bash
curl http://localhost:8080/api/tags          # list available models, via the proxy
```

#### Scenario 2 — Ollama on another machine

```bash
# .env
OLLAMA_UPSTREAM=http://192.168.1.50:11434
```

#### Scenario 3 — let the compose stack run Ollama for you

`docker-compose.yml` ships a commented-out `ollama` service. Uncomment it (and the
`depends_on: ollama` block under `proxy`), keep the default
`OLLAMA_UPSTREAM=http://ollama:11434`, then:

```bash
docker compose up --build

# pull a model once into the bundled Ollama
docker compose exec ollama ollama pull llama3
```

For GPU acceleration, also uncomment the `deploy.resources` block in that service.

#### Scenario 4 — running the proxy binary directly (no Docker)

```bash
go build -o ollama-proxy ./cmd/ollama-proxy-metrics/
./ollama-proxy \
  -listen   :8080 \
  -upstream http://127.0.0.1:11434 \
  -db       ./data/db.sqlite \
  -log      ./data/logs/proxy.log
```

### Pointing your Ollama clients at the proxy

Replace `:11434` with `:8080` everywhere:

| Client / tool          | Original                              | Through proxy                         |
|------------------------|---------------------------------------|---------------------------------------|
| curl                   | `http://localhost:11434/api/...`      | `http://localhost:8080/api/...`       |
| Open WebUI             | Server URL → `http://localhost:11434` | Server URL → `http://localhost:8080`  |
| LangChain / LlamaIndex | `base_url="http://localhost:11434"`   | `base_url="http://localhost:8080"`    |
| Ollama Python SDK      | `host="http://localhost:11434"`       | `host="http://localhost:8080"`        |
| Continue (VS Code ext) | `apiBase: http://localhost:11434`     | `apiBase: http://localhost:8080`      |
| OpenAI SDK (`/v1`)     | `base_url="http://localhost:11434/v1"`| `base_url="http://localhost:8080/v1"` |

## curl examples

### Generate (non-streaming)

```bash
curl -s http://localhost:8080/api/generate \
  -H "Content-Type: application/json" \
  -d '{
    "model":  "llama3",
    "prompt": "Why is the sky blue?",
    "stream": false
  }' | jq '{response, eval_count, prompt_eval_count}'
```

### Generate (streaming)

```bash
curl -N http://localhost:8080/api/generate \
  -H "Content-Type: application/json" \
  -d '{
    "model":  "llama3",
    "prompt": "Count from 1 to 5.",
    "stream": true
  }'
```

Each line is a JSON object; the last one has `"done": true` and contains token counts.

### Chat (non-streaming)

```bash
curl -s http://localhost:8080/api/chat \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3",
    "messages": [
      {"role": "system",    "content": "You are a helpful assistant."},
      {"role": "user",      "content": "What is 2 + 2?"}
    ],
    "stream": false
  }' | jq '.message.content'
```

### Chat (streaming)

```bash
curl -N http://localhost:8080/api/chat \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3",
    "messages": [{"role": "user", "content": "Tell me a joke."}],
    "stream": true
  }'
```

### With a session ID (enables per-session analytics in the dashboard)

```bash
curl -s http://localhost:8080/api/chat \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: alice" \
  -d '{
    "model":   "llama3",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream":  false
  }'
```

### Embeddings

```bash
curl -s http://localhost:8080/api/embed \
  -H "Content-Type: application/json" \
  -d '{
    "model": "nomic-embed-text",
    "input": "The quick brown fox"
  }' | jq '{model, .embeddings | length}'
```

### List available models

```bash
curl -s http://localhost:8080/api/tags | jq '[.models[].name]'
```

### OpenAI-compatible API (`/v1/*`)

The proxy also forwards and parses Ollama's OpenAI-compatible endpoints, so any
OpenAI client works by pointing its base URL at `http://localhost:8080/v1`.
Token counts are read from the response `usage` object.

```bash
# OpenAI chat completions (non-streaming)
curl -s http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: alice" \
  -d '{
    "model": "llama3",
    "messages": [{"role": "user", "content": "What is 2 + 2?"}]
  }' | jq '.choices[0].message.content'

# Streaming: add "stream": true (SSE). For token counts on streamed
# responses, ask the upstream to include usage:
#   "stream": true, "stream_options": {"include_usage": true}
```

### Check proxy is up

```bash
curl http://localhost:8080/          # info page
curl http://localhost:8080/metrics   # Prometheus metrics
curl http://localhost:8080/healthz   # liveness (always 200 if running)
curl http://localhost:8080/readyz    # readiness (200 if an upstream answers)
```

## Session tracking

| Source              | How to set                              | Recommended for         |
|---------------------|-----------------------------------------|-------------------------|
| `X-Session-ID` header | Pass in every request, any string     | Apps with named users   |
| Fallback            | Client IP address                       | CLI / ad-hoc usage      |

## Dashboard

Open **http://localhost:3000** after `docker compose up`.

The header has a **↻ Refresh** button (re-fetches all tabs) and a **🗑 Clear stats**
button that wipes every recorded request (calls `POST /admin/api/cleanup` after a
confirmation prompt).

### Overview tab

- Summary cards: total requests, prompt/completion/total tokens, **estimated cost**,
  avg latency, unique sessions, error count
- Daily bar chart: toggle between token usage, request counts, avg duration, **cost**;
  configurable time window (7/14/30/90 days)
- Top sessions quick-view

### Requests tab

- Paginated table of every request, newest first, with a **cost** column
- Filter by model, session ID, **free-text search** (prompt/response/endpoint),
  and a **date range**; **✕ clear** resets all filters
- **⚠ Errors only** narrows the table to failures — anything with an error
  message or a 4xx/5xx status — and the **status chips** under the filters show
  the full status-code distribution, each one clickable to isolate that code.
  The chips keep counting the codes you filtered out, so you can always see what
  you excluded
- Rows served from the response cache carry a **cache** pill; token counts the
  proxy estimated rather than read from the model are prefixed with **≈**
- **⤓ CSV** button exports the current (filtered) view; **● Live** streams new
  requests in real time via SSE (auto-pauses when you leave the tab). The live
  tail honors the error and status filters too
- Click a row to expand: the captured **prompt** and **response** text (each with
  a **copy** button), plus request ID, client IP, method, **TTFT**,
  **throughput (tok/s)**, cost, session, user-agent, token provenance, cache
  status, and any error details

### Models tab

- Per-model comparison: requests, prompt/completion/total tokens, avg duration,
  **avg TTFT**, **tokens/sec**, cost, and error count
- A horizontal bar chart ranks models by completion throughput

### Sessions tab

- Per-session breakdown: request count, prompt/completion/total tokens, avg
  duration, **cost**, first/last seen
- Click a session row to jump to the Requests tab pre-filtered to that session

## REST API

All endpoints return JSON. CORS `*` is enabled for local development.

| Method | Path                  | Description                          |
|--------|-----------------------|--------------------------------------|
| GET    | `/admin/api/summary`  | Overall aggregate statistics         |
| GET    | `/admin/api/requests` | Paginated request list               |
| GET    | `/admin/api/daily`    | Per-day aggregates                   |
| GET    | `/admin/api/sessions` | Per-session aggregates               |
| GET    | `/admin/api/models`   | Distinct model names seen            |
| GET    | `/admin/api/model-stats` | Per-model aggregates (tokens, TTFT, tok/s, cost) |
| GET    | `/admin/api/status-counts` | Status-code distribution for the current filters (the dashboard's error facet) |
| GET    | `/admin/api/budgets`  | Configured daily ceilings and today's per-session usage |
| GET    | `/admin/api/pricing`  | The configured pricing table         |
| GET    | `/admin/api/export`   | Download requests as CSV (default) or JSON (`?format=json`) |
| GET    | `/admin/api/stream`   | Server-Sent Events stream of new requests (live tail) |
| POST   | `/admin/api/cleanup`  | **Delete all** recorded requests (`DELETE` + `VACUUM`) |

> ⚠️ `cleanup` is destructive and unauthenticated. The whole `/admin/api/*`
> surface has no auth and sends `Access-Control-Allow-Origin: *`, so only expose
> the proxy on trusted networks. See [DESIGN.md](DESIGN.md#11-security--privacy).

### Query parameters

**`/admin/api/requests`**

| Param     | Default | Description                      |
|-----------|---------|----------------------------------|
| `limit`   | 50      | Rows per page (max 500)          |
| `offset`  | 0       | Pagination offset                |
| `model`   | —       | Filter by exact model name       |
| `session` | —       | Filter by session ID             |
| `q`       | —       | Case-insensitive substring across endpoint/model/session/prompt/response |
| `since`   | —       | Inclusive lower bound on timestamp (RFC3339) |
| `until`   | —       | Inclusive upper bound on timestamp (RFC3339) |
| `status`  | —       | Exact HTTP status code                       |
| `errors`  | `false` | `true` keeps only failures: an error message **or** a 4xx/5xx status |

**`/admin/api/daily`**

| Param  | Default | Description          |
|--------|---------|----------------------|
| `days` | 30      | Look-back window     |

**`/admin/api/sessions`**

| Param   | Default | Description          |
|---------|---------|----------------------|
| `limit` | 50      | Max sessions to return|

**`/admin/api/export`** — accepts `format` (`csv` | `json`), plus the same
`model` / `session` / `q` / `since` / `until` / `status` / `errors` filters as
`/requests`.

**`/admin/api/status-counts`** — accepts the same filters, but ignores `status`
and `errors`: a facet has to keep showing the codes you filtered out.

## SQLite schema

```sql
CREATE TABLE requests (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id        TEXT    NOT NULL UNIQUE,   -- random hex UUID
    session_id        TEXT    NOT NULL DEFAULT '',
    timestamp         TEXT    NOT NULL,          -- RFC3339Nano UTC
    endpoint          TEXT    NOT NULL,          -- e.g. /api/generate
    method            TEXT    NOT NULL DEFAULT 'POST',
    model             TEXT    NOT NULL DEFAULT '',
    stream            INTEGER NOT NULL DEFAULT 0,
    status_code       INTEGER NOT NULL DEFAULT 0,
    duration_ms       BIGINT  NOT NULL DEFAULT 0,
    ttft_ms           BIGINT  NOT NULL DEFAULT 0,   -- time to first token (streaming)
    request_bytes     BIGINT  NOT NULL DEFAULT 0,
    response_bytes    BIGINT  NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT  NOT NULL DEFAULT 0,
    completion_tokens BIGINT  NOT NULL DEFAULT 0,
    total_tokens      BIGINT  NOT NULL DEFAULT 0,
    cost              REAL    NOT NULL DEFAULT 0,   -- estimated cost from pricing table
    error_message     TEXT    NOT NULL DEFAULT '',
    client_ip         TEXT    NOT NULL DEFAULT '',
    user_agent        TEXT    NOT NULL DEFAULT '',
    prompt_text       TEXT    NOT NULL DEFAULT '',   -- captured prompt / last user message / embed input
    response_text     TEXT    NOT NULL DEFAULT '',   -- captured assistant response (concatenated across stream chunks)
    tokens_estimated  INTEGER NOT NULL DEFAULT 0,    -- 1 when the counts were estimated, not reported
    cached            INTEGER NOT NULL DEFAULT 0     -- 1 when served from the response cache
);

-- Indexes
CREATE INDEX idx_requests_timestamp  ON requests(timestamp);
CREATE INDEX idx_requests_session_id ON requests(session_id);
CREATE INDEX idx_requests_model      ON requests(model);
CREATE INDEX idx_requests_date       ON requests(substr(timestamp,1,10));
CREATE INDEX idx_requests_status     ON requests(status_code);
```

All token columns are `BIGINT` to support arbitrarily large cumulative counts.
The `ttft_ms`, `cost`, `prompt_text`, `response_text`, `tokens_estimated`, and
`cached` columns are added idempotently via `ALTER TABLE` on startup, so older
databases migrate forward automatically.

> **Privacy:** full prompt and response text is stored in plaintext in the
> SQLite DB. If that is undesirable, see [DESIGN.md](DESIGN.md#11-security--privacy).

## Prometheus metrics

```
ollama_proxy_requests_total{endpoint,model,status,stream}
ollama_proxy_request_duration_seconds{endpoint,model,stream}
ollama_proxy_time_to_first_token_seconds{endpoint,model}     # streaming only
ollama_proxy_request_bytes_in_total{endpoint,model,stream}
ollama_proxy_response_bytes_out_total{endpoint,model,stream}
ollama_proxy_prompt_tokens_total{endpoint,model}
ollama_proxy_completion_tokens_total{endpoint,model}
ollama_proxy_cost_total{model}                               # needs a pricing table
ollama_proxy_rate_limited_total                              # 429s from rate limiting
ollama_proxy_estimated_tokens_total{endpoint,model,kind}     # kind=prompt|completion
ollama_proxy_budget_denied_total{reason}                     # reason=tokens_per_day|cost_per_day
ollama_proxy_inflight_requests                               # requests being proxied right now
ollama_proxy_queue_wait_seconds                              # wait for a concurrency slot
ollama_proxy_queue_rejected_total                            # 503s from a full queue
ollama_proxy_cache_hits_total{endpoint,model}
ollama_proxy_cache_misses_total{endpoint,model}
ollama_proxy_cache_saved_tokens_total{endpoint,model}        # tokens not regenerated
ollama_proxy_cache_entries / ollama_proxy_cache_bytes        # only when the cache is on
ollama_proxy_upstream_up{upstream}                           # 1 = answered its last poll
ollama_proxy_upstream_requests_total{upstream,status}
ollama_proxy_upstream_retries_total{reason}                  # transport|status_5xx|model_not_found
```

Estimated tokens are deliberately **kept out of** `prompt_tokens_total` and
`completion_tokens_total`, which stay sourced from the model's own eval stats.
Cache hits likewise do not add to the token or cost counters — they add to
`cache_saved_tokens_total` instead, so `cost_total` keeps meaning "spent".

A pre-built Grafana dashboard wired to these metrics ships in
[`grafana/dashboards/ollama-proxy.json`](grafana/dashboards/ollama-proxy.json) —
see [Monitoring with Prometheus + Grafana](#monitoring-with-prometheus--grafana).

## JSON log format

Each request emits one JSON line to stdout **and** to `LOG_PATH`:

```json
{
  "time":              "2026-04-15T10:23:45.123Z",
  "level":             "INFO",
  "msg":               "request",
  "request_id":        "a3f1b2c4...",
  "session_id":        "127.0.0.1",
  "endpoint":          "/api/generate",
  "method":            "POST",
  "model":             "llama3",
  "stream":            false,
  "status_code":       200,
  "duration_ms":       1240,
  "ttft_ms":           180,
  "request_bytes":     312,
  "response_bytes":    4096,
  "prompt_tokens":     25,
  "completion_tokens": 180,
  "total_tokens":      205,
  "cost":              0.0,
  "tokens_estimated":  false,
  "cached":            false,
  "client_ip":         "127.0.0.1",
  "user_agent":        "curl/8.7.1",
  "error":             ""
}
```

Prompt and response *text* are persisted to SQLite only — never written to the log.

Parse with `jq`:

```bash
tail -f /data/logs/proxy.log | jq '{model, total_tokens, duration_ms}'
```

## Configuration

All flags have environment variable equivalents:

| Flag           | Env var           | Default                  | Description                                   |
|----------------|-------------------|--------------------------|-----------------------------------------------|
| `-listen`      | `LISTEN_ADDR`     | `:8080`                  | Listen address                                |
| `-upstream`    | `OLLAMA_UPSTREAM` | `http://127.0.0.1:11434` | Upstream URL(s); comma-separated for round-robin |
| `-db`          | `DB_PATH`         | `/data/db.sqlite`        | SQLite database path                          |
| `-log`         | `LOG_PATH`        | `/data/logs/proxy.log`   | JSON log file (empty = stdout only)           |
| `-static`      | `STATIC_DIR`      | `` (empty = info page)   | Serve a prebuilt frontend from Go             |
| `-pricing`     | `PRICING_PATH`    | `` (empty = no cost)     | JSON pricing table for cost estimation        |
| `-rate-limit`  | `RATE_LIMIT_RPM`  | `0`                      | Max requests per session per minute (0 = off) |
| `-log-max-mb`  | `LOG_MAX_MB`      | `50`                     | Rotate the log past this size (0 = no rotation) |
| `-log-backups` | `LOG_MAX_BACKUPS` | `5`                      | Rotated log files to retain                   |
| `-max-body-mb` | `MAX_BODY_MB`     | `32`                     | Reject request bodies larger than this (0 = no cap) |
| `-max-concurrent` | `MAX_CONCURRENT` | `0`                   | Requests forwarded at once (0 = no queueing)  |
| `-max-queue`   | `MAX_QUEUE`       | `0`                      | Requests allowed to wait for a slot (0 = unbounded) |
| `-cache-ttl`   | `CACHE_TTL`       | `0` (off)                | Cache identical non-streaming responses this long |
| `-cache-max-mb`| `CACHE_MAX_MB`    | `64`                     | Memory budget for the response cache          |
| `-budget-tokens` | `BUDGET_TOKENS_PER_DAY` | `0`            | Per-session daily token ceiling (0 = off)     |
| `-budget-cost` | `BUDGET_COST_PER_DAY` | `0`                  | Per-session daily cost ceiling (0 = off)      |
| `-normalize-models` | `NORMALIZE_MODELS` | `true`            | Record `llama3` as `llama3:latest`            |
| `-model-aliases` | `MODEL_ALIASES` | `` (empty)              | `alias=target,…` folding model names together |
| `-normalize-backfill` | `NORMALIZE_BACKFILL` | `false`     | Rewrite existing rows to normalized names at startup |
| `-inject-usage` | `INJECT_USAGE`   | `true`                   | Ask OpenAI-compatible streams to report usage |
| `-estimate-tokens` | `ESTIMATE_TOKENS` | `false`             | Estimate tokens from text when none are reported |
| `-retry-5xx`   | `RETRY_5XX`       | `true`                   | Retry a 5xx response on the next upstream     |
| `-upstream-poll` | `UPSTREAM_POLL` | `30s`                    | Model-inventory poll interval (0 = no routing) |
| `-circuit-failures` | `CIRCUIT_FAILURES` | `3`               | Consecutive failures before an upstream is skipped |
| `-circuit-cooldown` | `CIRCUIT_COOLDOWN` | `30s`             | How long a failing upstream is skipped        |
| `-shutdown-grace` | `SHUTDOWN_GRACE` | `30s`                  | Time in-flight requests get after `SIGTERM`   |

## Cost estimation

Point `-pricing` / `PRICING_PATH` at a JSON file of per-1,000-token rates and the
proxy stamps an estimated `cost` on every request, surfaced in the dashboard
(cost cards/columns), the `/admin/api/*` aggregates, and the
`ollama_proxy_cost_total{model}` metric. Local Ollama is free, so this is mainly
for "what would this traffic have cost on a hosted API" comparisons and chargeback.

```jsonc
// pricing.json — see pricing.example.json
{
  "currency": "USD",
  "default": { "prompt_per_1k": 0, "completion_per_1k": 0 },
  "models": {
    "gpt-4o": { "prompt_per_1k": 2.5, "completion_per_1k": 10 }
  }
}
```

Cost is computed once, at request time, and stored on the row — so historical
rows keep the price that was in effect when they ran. Rows recorded before a
pricing table was configured have `cost = 0`.

## Multiple upstreams (model-aware routing)

Set `OLLAMA_UPSTREAM` to a comma-separated list to spread traffic across several
Ollama backends:

```bash
OLLAMA_UPSTREAM="http://gpu-1:11434,http://gpu-2:11434,http://gpu-3:11434"
```

The proxy polls each backend's `/api/tags` every `UPSTREAM_POLL` and routes each
request to a backend that **actually has the requested model** — plain
round-robin would send a share of `qwen3:8b` traffic to a node that never pulled
it and collect 404s. Ordering is a preference, not a restriction: every upstream
stays in the candidate list, so a stale inventory degrades to round-robin rather
than failing the request.

On top of that:

- **Transport errors** fail over to the next candidate (as before).
- **5xx responses** are retried on the next candidate (`RETRY_5XX=false` to opt out).
- **404s** are retried when the inventory says another node serves that model.
- **Repeated failures** open a circuit: after `CIRCUIT_FAILURES` consecutive
  failures an upstream is skipped for `CIRCUIT_COOLDOWN`, then tried again.

Health and traffic per backend are exported as `ollama_proxy_upstream_up{upstream}`
and `ollama_proxy_upstream_requests_total{upstream,status}`.

## Concurrency limiting

Ollama serializes generation internally, so parallel clients do not go faster —
they thrash VRAM and inflate everyone's latency. `MAX_CONCURRENT` caps how many
requests are forwarded at once and queues the rest; `MAX_QUEUE` bounds that queue
so an overloaded proxy answers `503` with `Retry-After` instead of piling up
goroutines:

```bash
MAX_CONCURRENT=2 MAX_QUEUE=32
```

Queue behaviour is visible as `ollama_proxy_queue_wait_seconds`,
`ollama_proxy_queue_rejected_total`, and `ollama_proxy_inflight_requests`.

## Response cache

`CACHE_TTL` turns on an in-memory LRU cache for **non-streaming** requests, keyed
on the endpoint, method and exact request body:

```bash
CACHE_TTL=10m CACHE_MAX_MB=128
```

Only successful JSON responses are stored, never errors or streams. A hit is
answered without touching the upstream, carries `X-Proxy-Cache: hit` (misses get
`miss`), is flagged `cached` in the dashboard and the DB, and — because it burned
no GPU time — is charged to neither the cost counter nor the session's budget.
The tokens it saved land in `ollama_proxy_cache_saved_tokens_total`.

## Daily budgets

Rate limiting caps how *often* a session calls; a budget caps how *much* it
spends. Both ceilings are per session, over a UTC calendar day:

```bash
BUDGET_TOKENS_PER_DAY=200000 BUDGET_COST_PER_DAY=5.00
```

An exhausted session gets `429` with `Retry-After` set to the seconds remaining
until midnight UTC, and `ollama_proxy_budget_denied_total{reason}` increments.
Usage is held in memory but **seeded from SQLite at startup**, so a restart
resumes the day rather than handing out a fresh allowance — inspect it at
`/admin/api/budgets`.

Because a request's usage is only known once it finishes, the request that
crosses the line is allowed and the next one is refused.

## Model name normalization

Ollama treats `llama3` and `llama3:latest` as the same model, but clients send
both — splitting every aggregate and cost total in two. The proxy records the
normalized name (`NORMALIZE_MODELS=true`, the default), and `MODEL_ALIASES` folds
further names together:

```bash
MODEL_ALIASES="fast=qwen2.5:0.5b,big=llama3:70b"
```

Aliases affect **recording and routing only** — the request body is forwarded
untouched. Pricing tables keep working either way: a rate written for `llama3`
still prices rows recorded as `llama3:latest`, while an explicit `llama3:70b`
entry wins on exact match.

Existing rows keep their old names. Start once with `NORMALIZE_BACKFILL=true` to
rewrite them so history merges with new traffic.

## Token estimation

Token counts normally come from the model's own eval stats. Some responses carry
none — an OpenAI-compatible stream whose client did not ask for usage, an error
mid-generation — and recording those as "0 tokens, $0" makes them look free.

Two settings address that:

- `INJECT_USAGE` (default **on**) sets `stream_options.include_usage` on
  OpenAI-compatible streaming requests, so the upstream reports real counts. An
  explicit client choice is never overridden.
- `ESTIMATE_TOKENS` (default off) falls back to a ~4-characters-per-token
  estimate from the captured text. Estimated rows are flagged `tokens_estimated`,
  shown with a `≈` in the dashboard, and counted in
  `ollama_proxy_estimated_tokens_total` rather than the exact token counters.

## Request size limit

`MAX_BODY_MB` (default 32) caps the request body the proxy will buffer — bodies
must be buffered so they can be parsed and replayed on failover, so without a cap
one oversized POST can exhaust memory. Over-limit requests get `413` and are
recorded as errors. Set `0` to disable the cap.

## Graceful shutdown

On `SIGTERM`/`SIGINT` the proxy stops accepting connections and gives in-flight
requests up to `SHUTDOWN_GRACE` (default 30s) to finish — long generations and
open streams complete instead of being cut off — then closes the database and
flushes the log file.

## Rate limiting

Set `-rate-limit` / `RATE_LIMIT_RPM` to cap requests **per session** per minute
(session = `X-Session-ID` header, else client IP). Over-limit requests get
`429 Too Many Requests` with a `Retry-After` header and are still recorded (with
an error), and the `ollama_proxy_rate_limited_total` counter increments. `0`
(default) disables limiting. For a ceiling on *volume* rather than frequency, see
[Daily budgets](#daily-budgets).

## Monitoring with Prometheus + Grafana

Both are opt-in via the `monitoring` compose profile:

```bash
docker compose --profile monitoring up --build
```

- **Prometheus** → http://localhost:9090 (scrapes the proxy's `/metrics`)
- **Grafana** → http://localhost:3001 (anonymous admin; password `admin`)

Grafana is pre-provisioned: the Prometheus datasource and the **Ollama Proxy**
dashboard (`grafana/dashboards/ollama-proxy.json`) load automatically — request
rate, p95 latency, p95 TTFT, token throughput, cost rate, and totals.

## Running tests

```bash
go test ./...                    # all packages
go test ./internal/db/...  -v    # DB layer
go test ./internal/proxy/... -v  # proxy handler
go test ./internal/api/... -v    # REST API
```

## Frontend development

```bash
cd frontend
npm install
npm run dev      # http://localhost:5173 (proxies /admin/api → :8080)
```

Start the backend separately:

```bash
go run ./cmd/ollama-proxy-metrics/ \
  -db /tmp/dev.sqlite -log /tmp/proxy.log \
  -upstream http://127.0.0.1:11434
```

## Project layout

```
.
├── cmd/ollama-proxy-metrics/
│   └── main.go               # entry point: flags, logger, mux wiring
├── internal/
│   ├── db/                   # SQLite store: schema, migrations, queries
│   ├── proxy/                # reverse-proxy handler (native + OpenAI), metrics
│   ├── api/                  # REST API handlers (incl. export + SSE stream)
│   ├── pricing/             # per-model cost table + cost calculation
│   ├── events/              # in-process broker for the live (SSE) tail
│   ├── ratelimit/           # per-session fixed-window limiter
│   └── logging/             # size-based rotating log writer
├── frontend/                 # Vite + React + TypeScript + Recharts
│   ├── src/
│   │   ├── App.tsx
│   │   ├── api.ts            # typed API client
│   │   ├── format.ts        # shared cost/number/duration formatters
│   │   └── components/
│   │       ├── SummaryCards.tsx
│   │       ├── DailyChart.tsx
│   │       ├── RequestsTable.tsx
│   │       ├── ModelsTable.tsx
│   │       └── SessionsTable.tsx
│   ├── package.json
│   └── vite.config.ts
├── grafana/                  # provisioned datasource + dashboard JSON
├── pricing.example.json      # sample cost table (copy + set PRICING_PATH)
├── Dockerfile                # backend (CGO-free static binary)
├── Dockerfile.frontend       # node build → nginx
├── nginx.conf                # proxies /admin/api (+ SSE) → backend
├── docker-compose.yml
├── prometheus.yml
├── DESIGN.md                  # architecture & design rationale
└── README.md
```

## Design

For the architecture, request lifecycle, data model, and the reasoning behind the
trade-offs (CGO-free SQLite, WAL, metric cardinality, no auth, …), see
**[DESIGN.md](DESIGN.md)**.

## License

MIT
