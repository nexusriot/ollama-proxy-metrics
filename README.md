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
- **Multiple upstreams** (round-robin + failover) and optional **per-session rate limiting**

## Architecture

```
┌──────────────┐  /api/* /v1/*  ┌──────────────────────┐  upstream(s)  ┌────────────────┐
│  Client App  │ ────────────▶  │  ollama-proxy (Go)   │ ────────────▶ │  Ollama :11434 │
│  (curl / SDK)│                │  :8080               │  round-robin  │  external OR   │
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
round-robin with failover. By default that is an Ollama you run yourself; the
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
- Filter by model and session ID
- **⤓ CSV** button exports the current (filtered) view; **● Live** streams new
  requests in real time via SSE
- Click a row to expand: the captured **prompt** and **response** text, plus
  request ID, client IP, method, **TTFT**, **throughput (tok/s)**, cost, session,
  user-agent, and any error details

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

**`/admin/api/daily`**

| Param  | Default | Description          |
|--------|---------|----------------------|
| `days` | 30      | Look-back window     |

**`/admin/api/sessions`**

| Param   | Default | Description          |
|---------|---------|----------------------|
| `limit` | 50      | Max sessions to return|

**`/admin/api/export`** — accepts `format` (`csv` | `json`), plus `model` and
`session` filters (same semantics as `/requests`).

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
    response_text     TEXT    NOT NULL DEFAULT ''    -- captured assistant response (concatenated across stream chunks)
);

-- Indexes
CREATE INDEX idx_requests_timestamp  ON requests(timestamp);
CREATE INDEX idx_requests_session_id ON requests(session_id);
CREATE INDEX idx_requests_model      ON requests(model);
CREATE INDEX idx_requests_date       ON requests(substr(timestamp,1,10));
```

All token columns are `BIGINT` to support arbitrarily large cumulative counts.
The `ttft_ms`, `cost`, `prompt_text`, and `response_text` columns are added
idempotently via `ALTER TABLE` on startup, so older databases migrate forward
automatically.

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
```

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

## Multiple upstreams (load balancing)

Set `OLLAMA_UPSTREAM` to a comma-separated list to round-robin across several
Ollama backends, with automatic failover to the next on a connection error:

```bash
OLLAMA_UPSTREAM="http://gpu-1:11434,http://gpu-2:11434,http://gpu-3:11434"
```

## Rate limiting

Set `-rate-limit` / `RATE_LIMIT_RPM` to cap requests **per session** per minute
(session = `X-Session-ID` header, else client IP). Over-limit requests get
`429 Too Many Requests` with a `Retry-After` header and are still recorded (with
an error), and the `ollama_proxy_rate_limited_total` counter increments. `0`
(default) disables limiting.

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
