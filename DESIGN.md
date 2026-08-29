# Design — Ollama Proxy + Metrics

This document explains *how* and *why* the system is built the way it is. For
usage instructions see [README.md](README.md).

---

## 1. Purpose & scope

Ollama exposes an HTTP API but keeps **no durable history** of who called it,
how many tokens were spent, or how long requests took. This project sits
**transparently in front of Ollama** as a reverse proxy and turns every request
into three things:

1. a **Prometheus** time series (for dashboards/alerting),
2. a **structured JSON log line** (for `grep`/`jq`/log shippers),
3. a **row in SQLite** (for ad-hoc queries and the bundled dashboard).

The proxy must be **invisible to clients**: a request to `:8080/api/generate`
behaves exactly like a request to Ollama's `:11434/api/generate`, including
streaming semantics, headers, and status codes.

### Goals

- Zero client changes beyond swapping the base URL (`:11434` → `:8080`).
- Faithful pass-through of streaming responses (forward each chunk immediately).
- Accurate token accounting sourced from Ollama's own `eval_count` /
  `prompt_eval_count`, not a re-tokenizer.
- Single static binary, no CGO, trivial to containerize.
- Useful out of the box: a dashboard with no extra wiring.

### Non-goals

- **Not** an auth gateway (see §12).
- **Not** a long-term metrics store — Prometheus/Grafana own retention.
- **Not** a tokenizer. Counts come from the model; the optional estimator (§3) is
  a labelled fallback, never presented as exact.

Two former non-goals have since been taken on deliberately: the proxy now
**routes across several upstreams** by model and health, and it **enforces
quotas** (per-minute rate limits and daily token/cost budgets). Both live in
front of the upstream because that is the only place that sees every client.

---

## 2. System architecture

```
                        ┌──────────────────────────── ollama-proxy (Go, :8080) ───────────────────────────┐
                        │                                                                                  │
  client ──/api/*────▶  │  proxy.Handler ──▶ http.Client ──▶ upstream Ollama (OLLAMA_UPSTREAM)             │
                        │       │                                                                          │
                        │       ├──▶ Prometheus registry  ──────────────▶  GET /metrics                   │
                        │       ├──▶ slog JSON handler     ──────────────▶  stdout + LOG_PATH              │
                        │       └──▶ db.Store (SQLite/WAL) ──────────────▶  GET/POST /admin/api/*          │
                        │                                                          ▲                       │
                        └──────────────────────────────────────────────────────── │ ──────────────────────┘
                                                                                   │
                                          frontend (nginx, :3000) ── proxies /admin/api ─┘
```

One binary, a handful of small single-responsibility packages:

| Package            | Responsibility                                                        |
|--------------------|-----------------------------------------------------------------------|
| `cmd/…/main.go`    | Flag/env parsing, logger, DB open, pricing load, subsystem construction, mux wiring, listener + graceful `serve`, `/healthz` + `/readyz`. |
| `internal/proxy`   | The reverse-proxy `http.Handler` for `/api/*` and `/v1/*`; Prometheus metrics; request/response parsing (native + OpenAI); token/TTFT extraction; routing + retry; cache/gate/budget integration; cost stamping; event publish. |
| `internal/db`      | SQLite schema, migrations, insert, and all read queries (summary/daily/sessions/requests/model-stats/export/cleanup). |
| `internal/api`     | Read/admin REST handlers over `db.Store`, CORS, JSON encoding, CSV/JSON export, SSE stream. |
| `internal/pricing` | Per-model `$/1K-token` table + `Cost()` calculation; loaded from JSON. |
| `internal/events`  | In-process fan-out broker backing the live (SSE) tail; non-blocking, drop-on-slow. |
| `internal/ratelimit` | Per-session fixed-window request limiter (dependency-free). |
| `internal/logging` | Size-based rotating log writer (dependency-free).                     |
| `internal/upstream` | Upstream pool: model-inventory polling, per-request candidate ordering, circuit breaker. |
| `internal/gate`    | Concurrency semaphore with a bounded wait queue.                      |
| `internal/cache`   | TTL + size-bounded LRU for non-streaming responses.                   |
| `internal/budget`  | Per-session daily token/cost accounting, seeded from SQLite.          |
| `internal/modelname` | Model-name normalization and alias folding.                         |
| `internal/tokens`  | Rough token estimate used when the upstream reports none.             |

The HTTP surface is a single `http.ServeMux`:

| Prefix              | Handler                | Purpose                          |
|---------------------|------------------------|----------------------------------|
| `/api/`             | `proxy.Handler`        | Transparent Ollama native proxy  |
| `/v1/`              | `proxy.Handler`        | Transparent OpenAI-compatible proxy |
| `/metrics`          | `promhttp`             | Prometheus scrape endpoint       |
| `/healthz` `/readyz`| `main`                 | Liveness / readiness (readiness pings upstream) |
| `/admin/api/`       | `api.Handler`          | Dashboard REST API (+ `/export`, `/stream` SSE) |
| `/`                 | `FileServer` or info   | Static frontend (if `STATIC_DIR`) or a plain info page |

### Deployment topology

The default `docker-compose.yml` runs **two** services — `proxy` and `frontend`
(nginx) — and treats Ollama as **external**. A commented-out `ollama` service and
an opt-in `prometheus` service (`--profile monitoring`) are also provided. The
frontend container serves the compiled React bundle and reverse-proxies
`/admin/api` and `/metrics` to the backend, so the browser only talks to `:3000`.

---

## 3. Request lifecycle (the core of `proxy.go`)

Every `/api/*` and `/v1/*` request flows through `Handler.ServeHTTP`:

1. **Identity & timing.** Generate a random 128-bit hex `request_id`, capture
   `start`, derive `session_id` (§7) and `client_ip`.
2. **Rate limit (optional).** If enabled, `ratelimit.Limiter.Allow(session_id)`
   gates the request; over-limit requests get `429` + `Retry-After`, are recorded
   as errors, and bump `ollama_proxy_rate_limited_total`.
2a. **Budget (optional).** `budget.Tracker.Check(session_id)` compares the
   session's day-to-date tokens and cost against the configured ceilings. This
   runs *before* the body is read: an exhausted session should not get to spend
   memory either. Refusals are `429` with `Retry-After` set to the seconds left
   until midnight UTC.
3. **Buffer the request body**, wrapped in `http.MaxBytesReader` when
   `MAX_BODY_MB` is set, so it can be (a) JSON-parsed for `model`/`prompt`/`stream`
   and (b) replayed to the upstream (and to a second upstream on failover). The
   cap is what actually bounds per-request memory — buffering is unavoidable, so
   the size limit is the only defence against one oversized POST. Over-limit
   bodies get `413`.
3a. **Resolve the model name.** `modelname.Normalize` applies the alias map and
   the implicit `:latest` tag, so one model is one series. The *forwarded* body is
   never rewritten — aliases group statistics, they do not redirect requests.
4. **Best-effort parse** into a minimal `requestPayload` (`model`, `stream`,
   `prompt`, `messages`, `input`) — shapes that overlap between native and OpenAI.
   Parse failure is ignored; the request is still proxied.
5. **Determine `stream`** (`determineStream`): native generate/chat **default to
   `true`**; OpenAI (`/v1/*`) **defaults to `false`** (matching the OpenAI API);
   embeddings never stream; any non-`POST` (e.g. `GET /api/tags`, `GET /v1/models`)
   is non-stream.
5a. **Cache lookup** (non-streaming POSTs only, when `CACHE_TTL` is set), keyed
   on endpoint + method + exact body. A hit is replayed straight to the client
   with `X-Proxy-Cache: hit` and recorded with `cached=1`. The lookup happens
   *before* the concurrency gate on purpose: a hit costs no upstream work, so it
   has no business queueing behind requests that do.
5b. **Concurrency gate** (POSTs only, when `MAX_CONCURRENT` is set). Waiting time
   is observed into `ollama_proxy_queue_wait_seconds`; a full queue answers `503`
   + `Retry-After`; a client that disconnects while queued is recorded as `499`
   with nothing written.
5c. **Usage injection.** For OpenAI-compatible *streaming* requests,
   `stream_options.include_usage` is set unless the client chose explicitly —
   without it the upstream omits `usage` and the whole `/v1` streaming surface
   records zero tokens.
6. **Forward with routing and retry** (`forward`): `upstream.Pool.Pick(model)`
   returns *every* upstream, ordered — those known to serve the model first, then
   unknown inventories, then the rest, with an open circuit sinking one to the
   bottom. Candidates are tried in order on a transport error, on a 5xx (when
   `RETRY_5XX`), and on a 404 when the inventory says another node serves that
   model. Because the response headers are not written until `forward` returns, a
   retry is invisible to the client. The shared `http.Client` has **`Timeout: 0`**
   (a fixed timeout would kill long generations / open streams).
7. **Relay the response**, in one of two modes, format-aware (native vs OpenAI):

   - **Non-streaming** — read the whole body; for native, parse the single JSON
     object (with an NDJSON fallback for Ollama versions that stream even when
     `stream:false`); for OpenAI, read `choices[].message` + the `usage` object.
   - **Streaming** — scan line-by-line with a **1 MB max line buffer**, write each
     line (+`\n`) and `Flush()` immediately. The same loop preserves both NDJSON
     and SSE framing (blank lines between `data:` events round-trip intact). It
     accumulates response text, records **time-to-first-token** on the first
     non-empty chunk, and reads token counts from the final chunk (`done:true`
     for native; the `usage` object for OpenAI, when present).

8. **Record once.** Fill in estimated token counts if the upstream reported none
   and `ESTIMATE_TOKENS` is on, compute `cost` from the pricing table, increment
   Prometheus counters, build a `db.RequestRecord`, and call `persistAndLog` —
   one SQLite insert, one JSON log line, one budget charge, and one publish to
   live (SSE) subscribers.

Early failures (body read error, all upstreams unreachable, rate limit) short-circuit
through `recordError`, which still persists a row, logs, and publishes — so failed
requests are counted, not silently dropped.

### Token extraction

Tokens are **authoritative from Ollama**, taken from the final chunk's
`prompt_eval_count` (→ prompt/input tokens) and `eval_count` (→ completion
tokens). `total = prompt + completion`. If a `done` response carries neither
count, a warning is logged and the row stores `0` — unless `ESTIMATE_TOKENS` is
on, in which case `tokens.Estimate` fills in a ~4-chars-per-token guess from the
captured text and the row is flagged `tokens_estimated`. The flag is the whole
point: an estimate that cannot be told apart from a measurement is worse than no
estimate, so estimated counts are also kept out of `prompt_tokens_total` /
`completion_tokens_total` and reported under
`ollama_proxy_estimated_tokens_total` instead. This is why embeddings needed
a dedicated fix (they report `prompt_eval_count` only) and why the NDJSON
fallback exists.

### Text extraction

- **Prompt** — `prompt` for generate; the **last `user` message** for chat; the
  `input` (string or `[]string`, joined) for embeddings.
- **Response** — `response` for generate, `message.content` for chat,
  concatenated across all streamed chunks.

---

## 4. Data model

A single denormalized `requests` table (see [README → SQLite schema](README.md#sqlite-schema)).
Design choices:

- **One row per request**, written *after* the response finishes. There is no
  separate sessions/models table — sessions and model lists are derived at read
  time via `GROUP BY` / `DISTINCT`. For the expected scale (one host, human-driven
  traffic) this is simpler and fast enough with the right indexes.
- **`timestamp` stored as RFC3339Nano UTC text.** Lexical order == chronological
  order, so `ORDER BY timestamp` and `substr(timestamp,1,10)` day-bucketing work
  without date functions. Day grouping is indexed via an expression index.
- **Token/byte columns are `BIGINT`** to never overflow on cumulative reporting.
- **Migrations are additive & idempotent**: `CREATE TABLE IF NOT EXISTS` plus
  best-effort `ALTER TABLE … ADD COLUMN` (duplicate-column errors ignored). This
  is how `ttft_ms`, `cost`, `prompt_text`, and `response_text` were added without
  a migration tool.
- **`tokens_estimated` and `cached` are stored per row**, so every aggregate can
  be read two ways: as what clients consumed, or as what the GPU actually did.
  A cache hit records the tokens and cost the client would have paid (its row
  reads like any other) but is charged to neither `cost_total` nor the session
  budget, because nothing was generated.
- **Cost is stored on the row, computed once at write time** (a `cost REAL`
  column) rather than derived at read time. This keeps every aggregate a trivial
  `SUM(cost)` — no need to join a pricing table or group by model inside each
  query — and makes historical rows reflect the price in effect when they ran.
  The trade-off (accepted): re-pricing is not retroactive, and rows recorded
  before pricing was configured stay at 0. Similarly `ttft_ms` is precomputed so
  throughput aggregates need no per-row arithmetic in SQL.

### Read queries (`internal/db`)

| Method           | Backs endpoint      | Shape                                             |
|------------------|---------------------|---------------------------------------------------|
| `GetSummary`     | `/summary`          | All-time totals incl. `SUM(cost)`, avg duration, distinct sessions/models, error count. |
| `DailyStats`     | `/daily?days=N`     | Per-day sums (incl. cost) via `substr(timestamp,1,10)`, windowed by `datetime('now', -N days)`. |
| `SessionStats`   | `/sessions?limit`   | Per-`session_id` aggregates (incl. cost) ordered by last activity. |
| `ModelStats`     | `/model-stats`      | Per-`model` aggregates: tokens, avg duration, avg TTFT (non-zero), throughput, cost, errors. Excludes `''`/`unknown`. |
| `ListRequests`   | `/requests`         | Paginated rows + total count; optional `model`/`session` filters (parameterized). |
| `ExportRequests` | `/export`           | Same filters, no pagination, capped at 100k rows. |
| `Models`         | `/models`           | `DISTINCT model`.                                 |
| `StatusCounts`   | `/status-counts`    | Status-code distribution for the current filters, commonest first. |
| `UsageSince`     | — (startup)         | Per-session tokens/cost since a timestamp; seeds the budget tracker. |
| `RenameModel`    | — (startup)         | Rewrites one model name; backs the normalization backfill. |
| `DeleteAll`      | `/cleanup` (POST)   | `DELETE FROM requests` + `VACUUM`.                |

All user-supplied filters are passed as **bound parameters**, never string-concatenated.
Read/insert share a `selectColumns` constant + `scanRow`/`filterClause` helpers so
the column list lives in one place.

---

## 5. Observability: three sinks, one write

`persistAndLog` is the single fan-out point. Each sink answers a different question:

| Sink           | Best for                          | Retention owner       | Granularity        |
|----------------|-----------------------------------|-----------------------|--------------------|
| **Prometheus** | "rate / p95 / error% over time"   | Prometheus TSDB       | Aggregated series  |
| **JSON log**   | "stream/ship/grep individual events" | Your log pipeline  | Per request        |
| **SQLite**     | "browse & filter exact requests, incl. prompt/response text" | The DB file | Per request (full) |

### Metric cardinality

Labels are deliberately bounded:

```
ollama_proxy_requests_total{endpoint,model,status,stream}
ollama_proxy_request_duration_seconds{endpoint,model,stream}        # DefBuckets histogram
ollama_proxy_time_to_first_token_seconds{endpoint,model}            # histogram, streaming only
ollama_proxy_request_bytes_in_total / _out_total{endpoint,model,stream}
ollama_proxy_prompt_tokens_total / completion_tokens_total{endpoint,model}
ollama_proxy_cost_total{model}                                      # only when priced > 0
ollama_proxy_rate_limited_total                                     # no labels
ollama_proxy_estimated_tokens_total{endpoint,model,kind}
ollama_proxy_budget_denied_total{reason}
ollama_proxy_inflight_requests
ollama_proxy_queue_wait_seconds / ollama_proxy_queue_rejected_total
ollama_proxy_cache_hits_total / _misses_total / _saved_tokens_total{endpoint,model}
ollama_proxy_cache_entries / ollama_proxy_cache_bytes                # gauge funcs
ollama_proxy_upstream_up{upstream}
ollama_proxy_upstream_requests_total{upstream,status}
ollama_proxy_upstream_retries_total{reason}
```

The `upstream` label is bounded by the configured backend list, and `reason` /
`kind` by small enums, so cardinality stays flat.

`endpoint`, `model`, `status`, and `stream` are all low-cardinality. Crucially,
**`session_id` and `client_ip` are never used as labels** — they are unbounded
and would explode Prometheus. Per-session analysis lives in SQLite instead.
`cost_total` is incremented only when a pricing table yields a positive cost, so
a no-pricing deployment exports no `model`-labelled cost series.

The registry is a **fresh `prometheus.NewRegistry()`** (not the global default),
so the only series exported are the proxy's own — no Go runtime/process collectors
unless explicitly added. This keeps `/metrics` clean and makes tests hermetic.

---

## 6. Session identification

`extractSessionID` resolves, in order:

1. `X-Session-ID` request header (caller-supplied; any string), else
2. the **client IP**, via `X-Forwarded-For` (first hop) → `X-Real-IP` →
   `RemoteAddr` (port stripped).

This gives named-app users stable grouping while still bucketing anonymous CLI
traffic by origin. The `X-Forwarded-For`/`X-Real-IP` handling is what makes IPs
correct when the proxy itself sits behind nginx (as in the compose stack).

---

## 7. Frontend architecture

Vite + React + TypeScript + Recharts, no router and no state library:

- `api.ts` — a tiny typed `fetch` client; types mirror the Go JSON structs 1:1,
  plus `exportUrl()`/`streamUrl()` helpers and a `pricing()`/`modelStats()` call.
- `format.ts` — shared `fmtCost` (Intl currency), `fmtMs`, `fmtNum`.
- `App.tsx` — owns all state and data-loading; four tabs (`overview`, `requests`,
  `models`, `sessions`) plus header **Refresh** / **Clear stats**. It also fetches
  the pricing table once (for the currency symbol) and owns the live-tail state.
- `components/` — presentational: `SummaryCards`, `DailyChart` (toggle
  tokens/requests/duration/**cost** × 7/14/30/90-day window), `RequestsTable`
  (paginated; filter by model/session/**free-text search**/**date range**/**errors
  only**/**status chip**;
  expandable rows with prompt/response + **copy** buttons + TTFT/throughput/cost;
  **Live** and **CSV** controls), `ModelsTable` (per-model table + throughput bar
  chart), `SessionsTable` (click-through pre-filters the Requests tab).

Data flow is **pull, on demand**: load on mount, re-fetch when
filters/search/dates/status/page/window change (each filter change resets to page 1),
and a manual refresh button — *except* the live tail, which is **push**: toggling
**Live** opens an `EventSource` to `/admin/api/stream` and prepends rows as they
arrive (capped at 200, pagination suspended, still honoring the model/session/search
filters client-side). The stream is gated on the Requests tab, so switching tabs
pauses it and returning re-opens it. Search maps to the `q` param (case-insensitive
substring over endpoint/model/session/prompt/response, LIKE-wildcards escaped) and
the date pickers convert local time to UTC RFC3339 `since`/`until` bounds; both the
requests list and CSV/JSON export share the same filter set. A 25s SSE heartbeat
(`: ping`) keeps idle live connections alive through intermediaries.

**Two ways to serve it:** in production the nginx image serves the static build
and proxies `/admin/api`; in dev, `vite` proxies `/admin/api` → `:8080`. The Go
binary can also serve a prebuilt bundle directly via `-static`/`STATIC_DIR`.

---

## 8. Key dependencies & why

| Dependency                  | Why this one                                                       |
|-----------------------------|-------------------------------------------------------------------|
| `modernc.org/sqlite`        | **Pure-Go, CGO-free** SQLite → `CGO_ENABLED=0` static binary, tiny Alpine image, trivial cross-compile. Trades some raw speed vs `mattn/go-sqlite3`, irrelevant at this scale. |
| `prometheus/client_golang`  | De-facto standard metrics library; first-class histogram/counter vecs. |
| `log/slog` (stdlib)         | Structured JSON logging without a third-party logger.             |
| Recharts                    | Declarative charts that fit React; no D3 hand-rolling.            |

SQLite runs in **WAL mode** (`PRAGMA journal_mode=WAL`) for better read/write
concurrency — readers (the dashboard API) don't block the writer (the proxy).

**No new external dependencies** were added for the Tier 2/3 work. Rate limiting,
log rotation, the SSE broker, and pricing are all small standard-library
implementations (`internal/ratelimit`, `internal/logging`, `internal/events`,
`internal/pricing`) rather than pulling in `x/time/rate`, `lumberjack`, etc. — it
keeps the offline `CGO_ENABLED=0` build trivial and the dependency surface tiny,
at the cost of fewer features (fixed-window not token-bucket; size-only rotation).

---

## 9. Concurrency & performance

- The proxy is naturally concurrent: Go's HTTP server runs each request in its
  own goroutine; a **single shared `http.Client`** pools upstream connections.
- **No request timeout** on the upstream client — required for long generations
  and open streams. The trade-off is that a hung upstream holds a goroutine until
  the client disconnects (request context cancellation does propagate).
- **Request bodies are fully buffered** in memory (needed for parse + replay);
  **response bodies are buffered only for non-streaming** requests. Streaming
  responses are relayed chunk-by-chunk and never fully held.
- The streaming scanner caps lines at **1 MB**; a single larger-than-1 MB NDJSON
  line would error the scan (recorded in `error_message`). Normal Ollama chunks
  are far smaller.
- SQLite writes are one INSERT per request on the request's own goroutine; WAL
  keeps this from blocking dashboard reads.

---

## 10. Configuration

Everything is a flag with an env fallback (flag wins if set), resolved in
`main.go`:

| Flag           | Env               | Default                   | Notes                          |
|----------------|-------------------|---------------------------|--------------------------------|
| `-listen`      | `LISTEN_ADDR`     | `:8080`                   |                                |
| `-upstream`    | `OLLAMA_UPSTREAM` | `http://127.0.0.1:11434`  | Comma-separated → round-robin + failover. Binary default differs from the image default (`http://ollama:11434`). |
| `-db`          | `DB_PATH`         | `/data/db.sqlite`         | Parent dir auto-created.       |
| `-log`         | `LOG_PATH`        | `/data/logs/proxy.log`    | Empty → stdout only.           |
| `-static`      | `STATIC_DIR`      | `` (info page)            | Serve a prebuilt frontend from Go. |
| `-pricing`     | `PRICING_PATH`    | `` (no cost)              | JSON pricing table; a non-empty path that fails to load is fatal. |
| `-rate-limit`  | `RATE_LIMIT_RPM`  | `0`                       | Per-session requests/min; 0 disables. |
| `-log-max-mb`  | `LOG_MAX_MB`      | `50`                      | Rotate past this size; 0 disables rotation. |
| `-log-backups` | `LOG_MAX_BACKUPS` | `5`                       | Rotated files retained.        |
| `-max-body-mb` | `MAX_BODY_MB`     | `32`                      | Body cap; 0 disables.          |
| `-max-concurrent` / `-max-queue` | `MAX_CONCURRENT` / `MAX_QUEUE` | `0` / `0` | Concurrency slots and queue depth. |
| `-cache-ttl` / `-cache-max-mb` | `CACHE_TTL` / `CACHE_MAX_MB` | `0` / `64` | Response cache; a zero TTL disables it. |
| `-budget-tokens` / `-budget-cost` | `BUDGET_TOKENS_PER_DAY` / `BUDGET_COST_PER_DAY` | `0` / `0` | Per-session daily ceilings. |
| `-normalize-models` | `NORMALIZE_MODELS` | `true`               | Record `llama3` as `llama3:latest`. |
| `-model-aliases` | `MODEL_ALIASES`  | `` (none)                 | `alias=target,…`.              |
| `-normalize-backfill` | `NORMALIZE_BACKFILL` | `false`         | One-shot rewrite of historical rows. |
| `-inject-usage` | `INJECT_USAGE`   | `true`                    | Set `stream_options.include_usage` on `/v1` streams. |
| `-estimate-tokens` | `ESTIMATE_TOKENS` | `false`                | Estimate tokens when none are reported. |
| `-retry-5xx`   | `RETRY_5XX`       | `true`                    | Retry a 5xx on the next upstream. |
| `-upstream-poll` | `UPSTREAM_POLL` | `30s`                     | Inventory poll; 0 disables routing. |
| `-circuit-failures` / `-circuit-cooldown` | `CIRCUIT_FAILURES` / `CIRCUIT_COOLDOWN` | `3` / `30s` | Circuit-breaker trip and recovery. |
| `-shutdown-grace` | `SHUTDOWN_GRACE` | `30s`                   | Time in-flight requests get on SIGTERM. |

---

## 11. Security & Privacy

> The proxy is designed for a **trusted local/LAN deployment**. It is **not**
> hardened for hostile networks.

Current posture and the reasoning behind it:

- **No authentication anywhere.** Both the Ollama proxy (`/api/*`) and the admin
  API (`/admin/api/*`) are open. `/admin/api/cleanup` can wipe all data with a
  single unauthenticated `POST`, and `/admin/api/budgets` discloses per-session
  usage.
- **CORS is `*`** on the admin API to make local dev frictionless.
- **Full prompt & response text is stored in plaintext** in SQLite
  (`prompt_text` / `response_text`) and is returned by `/admin/api/requests`.
  Prompts/responses are **not** written to the JSON log, only to the DB.
- The DB file under `./data/` is git-ignored and (now) docker-ignored, so it is
  not committed or shipped into the image — but it is unencrypted on disk.

**If deploying beyond a trusted host**, the recommended mitigations are: put an
authenticating reverse proxy in front (or add an API-key middleware), restrict
CORS to the dashboard origin, bind the admin API to localhost only, and consider
making prompt/response capture opt-out or length-capped (see §12, Tier 1).

---

## 12. Roadmap status & future directions

### Implemented — Tier 2 (observability depth)

- **TTFT & tokens/sec** — `ttft_ms` per row + `ollama_proxy_time_to_first_token_seconds`
  histogram; throughput derived as completion tokens / generation time.
- **Cost estimation** — `internal/pricing` table; cost stamped per row, aggregated
  in summary/daily/sessions/model-stats, and exported as `ollama_proxy_cost_total`.
- **Per-model dashboard tab** — `/admin/api/model-stats` + the Models view.
- **Grafana dashboard** — provisioned datasource + dashboard JSON under `grafana/`.
- **Live tail** — `internal/events` broker → `/admin/api/stream` (SSE) → dashboard toggle.
- **Export** — `/admin/api/export` (CSV/JSON, honoring filters).

### Implemented — Tier 3 (proxy capabilities)

- **OpenAI-compatible routes** — `/v1/*` proxied and parsed (chat/completions/
  embeddings), reading the `usage` object and handling SSE framing.
- **Multiple upstreams** — comma-separated `OLLAMA_UPSTREAM`, round-robin + failover.
- **Per-session rate limiting** — `internal/ratelimit`, `RATE_LIMIT_RPM`.
- **Log rotation** — `internal/logging` size-based rotation (`LOG_MAX_MB`/`LOG_MAX_BACKUPS`).
- **`/healthz` + `/readyz`** — readiness pings an upstream's `/api/version`.

### Implemented — Tier 4 (capacity, correctness & spend control)

- **Graceful shutdown** — `signal.NotifyContext` + `http.Server.Shutdown` behind a
  `run() error` so every deferred close actually runs. Previously a `SIGTERM` cut
  open streams mid-generation and skipped the DB close and log flush entirely.
- **Request body cap** — `http.MaxBytesReader` + `MAX_BODY_MB`, answering `413`.
  §9 claimed memory was bounded by request size; until this there was no bound.
- **Model-aware routing, retry and circuit breaking** — `internal/upstream`.
- **Concurrency gate** — `internal/gate`, bounded queue, `503` when full.
- **Response cache** — `internal/cache`, non-streaming only, `X-Proxy-Cache`.
- **Daily token/cost budgets** — `internal/budget`, seeded from SQLite at startup.
- **Model normalization + aliases** — `internal/modelname`, with an opt-in backfill.
- **Usage injection & token estimation** — `stream_options.include_usage` for `/v1`
  streams; a flagged estimate when the upstream still reports nothing.
- **Error-focused dashboard view** — errors-only filter plus a status-code facet
  (`/admin/api/status-counts`), shared by the list, the live tail and the export.

### Still open — Tier 1 (safety & correctness gaps)

These remain the highest-value next steps and are deliberately **not** implemented:

- **Admin API authentication** + configurable CORS origin; bind `/admin/api` and
  `/metrics` separately from `/api`. The live-tail and export endpoints widen the
  data exposure, making this more pressing than before.
- **Privacy controls for capture** — env flag to disable prompt/response storage,
  a max-length cap, and/or redaction patterns.
- **Data retention / auto-pruning** — delete rows older than N days (today
  `cleanup` is all-or-nothing); a `DELETE … WHERE timestamp <` job.

### Known limitations of the new features

- **OpenAI streaming token counts** require the client to send
  `stream_options.include_usage=true`; without it the upstream omits `usage` and
  tokens are recorded as 0 (text and latency are still captured).
- **Rate limiting is per-instance and in-memory** — counts reset on restart and
  are not shared across replicas. Fixed-window, so it allows brief bursts at
  window edges.
- **Cost is frozen at request time**; changing the pricing table does not reprice
  historical rows (a deliberate trade for trivial SQL aggregation — see §4).
- **Retries** cover transport errors, 5xx, and model-not-found 404s, all before
  any response byte reaches the client. A failure *after* the stream has started
  is still terminal — the client already has half an answer.
- **Routing is only as fresh as the last inventory poll.** A model pulled or
  deleted mid-interval is routed on stale information; the 404 retry is the
  safety net, and ordering never excludes an upstream outright.
- **The concurrency gate and the rate limiter are per-instance.** Like the
  limiter, budgets live in memory — seeded from SQLite at startup, but not shared
  across replicas.
- **Budgets allow one overshooting request.** Usage is only known once a request
  finishes, so the request that crosses the line completes and the next is refused.
- **The response cache keys on the exact request body.** A semantically identical
  request with reordered JSON keys is a miss, by design — parsing and canonicalizing
  every body would cost more than the cache saves.

---

## 13. Testing

Go tests per package, all CGO-free and hermetic (every store opens `:memory:`):

- `internal/db` — schema/insert (incl. duplicate-ID, int64 tokens), pagination +
  model/session filtering, **free-text `q` search (with LIKE-escaping) and
  `since`/`until` date-range filtering**, `ModelStats` (ordering + `unknown`
  exclusion), export (filter + limit), and cost aggregation in the summary.
- `internal/proxy` — an `httptest` fake upstream exercises native non-stream and
  stream proxying, **OpenAI non-stream + SSE** parsing (usage + framing), token
  extraction, status recording, `502`, **multi-upstream failover**, **rate-limit
  `429`**, **cost recording**, `determineStream` defaults, session/IP resolution,
  the `model:"unknown"` default, and byte accounting.
- `internal/api` — every handler via `httptest`: CORS/preflight, method guards,
  cleanup, model-stats, CSV + JSON export, pricing, **free-text search (`q`)**, and
  the SSE stream (both the 503-without-broker path and live delivery against a real
  `httptest.Server`).
- `internal/pricing`, `internal/ratelimit`, `internal/events`, `internal/logging`
  — unit tests for cost math/loading (including the untagged-rate fallback that
  keeps existing pricing files working under normalization), window/limit/nil-safety,
  non-blocking fan-out, and size-triggered rotation.
- `internal/upstream`, `internal/gate`, `internal/cache`, `internal/budget`,
  `internal/modelname`, `internal/tokens` — the new subsystems in isolation:
  candidate ordering and inventory polling against `httptest` upstreams, circuit
  trip/expiry on a fake clock, queue-full and double-release safety, LRU eviction
  and TTL expiry on a fake clock, day rollover and seeding, tag/alias handling.
- `internal/proxy` (`features_test.go`) — each new behaviour end-to-end through
  `ServeHTTP`: the `413` cap, normalization and aliases, usage injection,
  estimation on and off, cache hit/miss/never-for-streams, gate serialization and
  `503`, budget refusal and accrual, and 5xx / model-not-found retries.
- `cmd/ollama-proxy-metrics` — `serve` finishes an in-flight request after the
  context is cancelled, stops accepting afterwards, and surfaces an expired grace
  period; plus budget seeding, the normalization backfill and the env helpers.

The whole suite passes under the race detector (`go test -race ./...`).

Remaining gap: the NDJSON-when-`stream:false` native fallback and the OpenAI
embeddings path still lack dedicated cases, and the frontend has no tests at all.

```bash
go test -race ./...    # all green; see README for per-package invocations
go vet ./...           # clean
```
