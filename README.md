# Ollama Proxy + Metrics (PoC)

A lightweight **Go-based reverse proxy** for Ollama that adds
**Prometheus-compatible metrics**, making it possible to observe:

-   request counts
-   latency histograms
-   bytes in/out
-   prompt tokens & completion tokens (when `stream: false`)
-   per-model and per-endpoint statistics

This is a **Proof of Concept (PoC)** and not a production-hardened
component.
It demonstrates how to intercept Ollama API traffic and extract
performance + usage metrics.

Features

-   **Transparent proxy** for Ollama `/api/*` endpoints
-   **Prometheus `/metrics` endpoint**
-   Metrics labeled by:
    -   `model`
    -   `endpoint`
    -   `status`
    -   `stream` (true/false)
-   Extracts Ollama's built-in `eval_count`, `prompt_eval_count` fields
-   Supports both **streaming** and **non-streaming** responses
-   Minimal code (\~260 LOC), easy to extend or embed

Requirements

-   Go 1.24
-   Running Ollama instance (default: `http://127.0.0.1:11434`)
-   Optional: Prometheus + Grafana stack for dashboards

## Getting Started

### Clone and build

``` bash
git clone https://github.com/nexusriot/ollama-proxy-metrics
cd ollama-proxy-metrics

go mod tidy
go build -o ollama-proxy .
```

### Run (default settings)

``` bash
./ollama-proxy
```

This starts:

-   Proxy on: **http://localhost:8080**
-   Upstream Ollama: **http://127.0.0.1:11434**

### Override upstream

``` bash
OLLAMA_UPSTREAM="http://192.168.88.45:11434" ./ollama-proxy
```

Or via flags:

``` bash
./ollama-proxy -listen ":9000" -upstream "http://ollama:11434"
```

## Using the Proxy

Send requests to the proxy instead of Ollama directly:

``` bash
curl -s http://localhost:8080/api/generate   -H "Content-Type: application/json"   -d '{
    "model": "codellama:34b",
    "prompt": "Generate Hello world on python",
    "stream": false
  }'
```

## Metrics

Metrics are exposed for Prometheus at:

    http://localhost:8080/metrics

Example metrics:

    ollama_proxy_requests_total{endpoint="/api/generate",model="codellama:34b",status="200",stream="false"} 5
    ollama_proxy_request_duration_seconds_bucket{endpoint="/api/generate",model="llama3",stream="true",le="0.5"} 2
    ollama_proxy_prompt_tokens_total{endpoint="/api/generate",model="llama3"} 128
    ollama_proxy_completion_tokens_total{endpoint="/api/generate",model="llama3"} 256

##  Architecture Overview (PoC)

       ┌────────────┐       ┌───────────────────┐       ┌───────────────┐
       │ Client App │ --->  │  Ollama Proxy     │ --->  │   Ollama       │
       │ (curl/UI)  │       │ (Go, metrics)     │       │  11434 API     │
       └────────────┘       └─────────┬─────────┘       └───────────────┘
                                       │
                                 /metrics (Prometheus)

## Project Layout

    .
    ├── go.mod
    ├── go.sum
    ├── cmd/ollama-proxy-metrics/main.go        # Core proxy + metrics logic
    └── README.md

## Limitations (as PoC)

-   No TLS support
-   No header filtering
-   No throttling
-   No structured logs
-   No circuit breaking
-   No token metrics for streaming mode

## Roadmap(?)

-   Grafana dashboard
-   OpenTelemetry tracing
-   Persistent usage logs
-   Docker images
-   Rate limiting

## Testing

### Non-streaming test

``` bash
curl -s http://localhost:8080/api/generate   -H "Content-Type: application/json"   -d '{"model":"llama3", "prompt":"Hi", "stream":false}'
```

### Streaming test

``` bash
curl -N http://localhost:8080/api/generate   -H "Content-Type: application/json"   -d '{"model":"llama3", "prompt":"Hi", "stream":true}'
```

## License

MIT License
