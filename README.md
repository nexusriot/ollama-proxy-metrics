# Ollama Proxy + Metrics

A lightweight **Go reverse proxy** for Ollama that adds:

- **Per-request SQLite persistence** — tokens, latency, bytes, session, error
- **Structured JSON logging** to a rotating log file
- **Prometheus `/metrics`** endpoint (existing)
- **REST API** for the dashboard (`/admin/api/*`)
- **React dashboard** — per-request table, per-day charts, per-session breakdown

## Architecture

```
┌──────────────┐    /api/*     ┌──────────────────────┐    ┌────────────────┐
│  Client App  │ ──────────▶  │  ollama-proxy (Go)   │ ──▶│  Ollama :11434 │
│  (curl / UI) │              │  :8080               │    └────────────────┘
└──────────────┘              │                      │
                              │  /metrics  (Prom)    │
                              │  /admin/api/* (REST) │
                              │  SQLite + log file   │
                              └──────────────────────┘
                                        ▲
                              ┌─────────┴────────┐
                              │  frontend (nginx) │
                              │  :3000            │
                              └──────────────────┘
```

## Ports at a glance

| Port  | Service    | Exposed by      | What it serves                                     |
|-------|------------|-----------------|----------------------------------------------------|
| 8080  | proxy      | host ↔ container| Ollama reverse proxy (`/api/*`), Prometheus (`/metrics`), REST API (`/admin/api/*`) |
| 3000  | frontend   | host ↔ container| React metrics dashboard (nginx)                    |
| 11434 | ollama     | host ↔ container| Ollama HTTP API (bundled in compose)               |
| 9090  | prometheus | host ↔ container| Prometheus UI — opt-in (`--profile monitoring`)    |

> **Rule of thumb:** point all your Ollama clients at **`:8080`** instead of `:11434`.
> The proxy is 100% transparent — every request is forwarded unchanged.

## Quick Start

### Docker Compose (recommended)

```bash
git clone https://github.com/nexusriot/ollama-proxy-metrics
cd ollama-proxy-metrics

docker compose up --build
```

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

#### Scenario 1 — bundled Ollama (default compose)

The `ollama` service is included in `docker-compose.yml` and starts automatically.
Pull a model once, then use the proxy:

```bash
# pull a model into the bundled Ollama
docker compose exec ollama ollama pull llama3

# send requests through the proxy (not directly to :11434)
curl http://localhost:8080/api/tags          # list available models
```

#### Scenario 2 — external Ollama on the same host

If Ollama is already running on your machine (`localhost:11434`), remove or
comment out the `ollama` service in `docker-compose.yml` and point the proxy at
the host network:

```yaml
# docker-compose.yml
proxy:
  environment:
    OLLAMA_UPSTREAM: "http://host.docker.internal:11434"   # Mac / Windows
    # OLLAMA_UPSTREAM: "http://172.17.0.1:11434"           # Linux (docker bridge gateway)
```

#### Scenario 3 — external Ollama on another machine

```yaml
proxy:
  environment:
    OLLAMA_UPSTREAM: "http://192.168.1.50:11434"
```

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

### Check proxy is up

```bash
curl http://localhost:8080/          # info page
curl http://localhost:8080/metrics   # Prometheus metrics
```

## Session tracking

| Source              | How to set                              | Recommended for         |
|---------------------|-----------------------------------------|-------------------------|
| `X-Session-ID` header | Pass in every request, any string     | Apps with named users   |
| Fallback            | Client IP address                       | CLI / ad-hoc usage      |

## Dashboard

Open **http://localhost:3000** after `docker compose up`.

### Overview tab

- Summary cards: total requests, prompt/completion/total tokens, avg latency,
  unique sessions, error count
- Daily bar chart: toggle between token usage, request counts, avg duration;
  configurable time window (7/14/30/90 days)
- Top sessions quick-view

### Requests tab

- Paginated table of every request, newest first
- Filter by model and session ID
- Click a row to expand: request ID, client IP, user-agent, error details

### Sessions tab

- Per-session breakdown: request count, prompt/completion/total tokens, avg
  duration, first/last seen
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
    request_bytes     BIGINT  NOT NULL DEFAULT 0,
    response_bytes    BIGINT  NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT  NOT NULL DEFAULT 0,
    completion_tokens BIGINT  NOT NULL DEFAULT 0,
    total_tokens      BIGINT  NOT NULL DEFAULT 0,
    error_message     TEXT    NOT NULL DEFAULT '',
    client_ip         TEXT    NOT NULL DEFAULT '',
    user_agent        TEXT    NOT NULL DEFAULT ''
);
```

All token columns are `BIGINT` to support arbitrarily large cumulative counts.

## Prometheus metrics

```
ollama_proxy_requests_total{endpoint,model,status,stream}
ollama_proxy_request_duration_seconds{endpoint,model,stream}
ollama_proxy_request_bytes_in_total{endpoint,model,stream}
ollama_proxy_response_bytes_out_total{endpoint,model,stream}
ollama_proxy_prompt_tokens_total{endpoint,model}
ollama_proxy_completion_tokens_total{endpoint,model}
```

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
  "request_bytes":     312,
  "response_bytes":    4096,
  "prompt_tokens":     25,
  "completion_tokens": 180,
  "total_tokens":      205,
  "client_ip":         "127.0.0.1",
  "user_agent":        "curl/8.7.1",
  "error":             ""
}
```

Parse with `jq`:

```bash
tail -f /data/logs/proxy.log | jq '{model, total_tokens, duration_ms}'
```

## Configuration

All flags have environment variable equivalents:

| Flag        | Env var          | Default                        |
|-------------|------------------|--------------------------------|
| `-listen`   | `LISTEN_ADDR`    | `:8080`                        |
| `-upstream` | `OLLAMA_UPSTREAM`| `http://127.0.0.1:11434`       |
| `-db`       | `DB_PATH`        | `/data/db.sqlite`              |
| `-log`      | `LOG_PATH`       | `/data/logs/proxy.log`         |
| `-static`   | `STATIC_DIR`     | `` (empty = info page)         |

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
│   ├── db/
│   │   ├── db.go             # SQLite store: schema, insert, queries
│   │   └── db_test.go
│   ├── proxy/
│   │   ├── proxy.go          # reverse-proxy handler + Prometheus metrics
│   │   └── proxy_test.go
│   └── api/
│       ├── api.go            # REST API handlers for the dashboard
│       └── api_test.go
├── frontend/                 # Vite + React + TypeScript + Recharts
│   ├── src/
│   │   ├── App.tsx
│   │   ├── api.ts            # typed API client
│   │   └── components/
│   │       ├── SummaryCards.tsx
│   │       ├── DailyChart.tsx
│   │       ├── RequestsTable.tsx
│   │       └── SessionsTable.tsx
│   ├── package.json
│   └── vite.config.ts
├── Dockerfile                # backend (CGO-free static binary)
├── Dockerfile.frontend       # node build → nginx
├── nginx.conf                # proxies /admin/api → backend
├── docker-compose.yml
├── prometheus.yml
└── README.md
```

## License

MIT
