# Distributed Tracing

Both the controller and the in-process L7 proxy can export OpenTelemetry traces. The feature is **opt-in** and **off by default** — when disabled there is zero cost on the request hot path and outbound trace headers are forwarded unchanged.

## Why

Per-request logs and Prometheus metrics answer "what happened" and "how often", but not "where did this one slow request spend its time". With tracing enabled, a request can be followed end to end — Cloudflare edge → in-cluster proxy → backend — as a single trace, and the existing structured logs gain `trace_id` / `span_id` fields so a log line links directly to its span.

The controller side traces its outbound calls (Cloudflare API, config push to proxy replicas), which is useful when diagnosing slow reconciles or Cloudflare API latency.

## Span model

When tracing is enabled on the proxy:

- Each inbound request gets a **server span** (`SpanKind=Server`). If the request already carries a W3C `traceparent` (injected upstream), the server span is parented to it; otherwise it starts a new trace subject to sampling.
- Each backend call gets a **client span** (`SpanKind=Client`) that is a child of the server span. The proxy injects the client span's context into `traceparent` / `tracestate` on the outbound request, so a trace-aware backend continues the same trace.
- The server span records the response status code and marks 5xx as an error. WebSocket upgrades (`101`) are tagged rather than timed, since the span would otherwise span the whole session.

When tracing is enabled on the controller, its Cloudflare API client and its proxy config-push client each emit a client span. Reconciles do not run inside an inbound trace, so these are typically root spans rather than children of a reconcile trace; they still carry the call's latency and the `trace_id` / `span_id` log correlation.

## Enabling

### Proxy (Helm)

```yaml
proxy:
  tracing:
    enabled: true
    endpoint: "otel-collector.observability:4317"
    sampleRate: 1.0
```

### Controller (Helm)

```yaml
controller:
  tracing:
    enabled: true
    endpoint: "otel-collector.observability:4317"
    sampleRate: 1.0
```

The two are independent: the proxy is the data plane (server + backend spans), the controller only produces spans for its own outbound API calls. Enable both, pointing them at the same collector, for a complete picture.

### Environment variables and flags

Without the Helm chart, the proxy reads environment variables:

```bash
PROXY_TRACING_ENABLED=true \
PROXY_TRACING_ENDPOINT=otel-collector.observability:4317 \
PROXY_TRACING_SAMPLE_RATE=0.1 \
  proxy
```

The controller reads cobra flags (or `CF_`-prefixed env, e.g. `CF_TRACING_ENABLED`):

```bash
controller --tracing-enabled=true \
  --tracing-endpoint=otel-collector.observability:4317 \
  --tracing-sample-rate=0.1
```

Truthy forms for `PROXY_TRACING_ENABLED`: `1`, `true`, `TRUE` (case-insensitive, whitespace ignored). Any other value disables the feature.

## Endpoint precedence

The endpoint value selects the transport:

- A **bare host:port** (e.g. `otel-collector.observability:4317`) uses plaintext gRPC — the common in-cluster collector setup.
- A value with an **`http://` or `https://` scheme** selects plaintext vs TLS explicitly.
- An **empty endpoint** defers to the standard `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` environment variables, so operators who already set the OpenTelemetry SDK conventions get them honored without a chart value.

Export is over OTLP/gRPC.

## Sampling

`sampleRate` is a probability in `[0, 1]` applied at the **trace root** via `ParentBased(TraceIDRatioBased)`:

- `1.0` samples every trace.
- `0.1` samples roughly a tenth.
- An upstream sampling decision (carried in `traceparent`) is **respected** — the ratio only applies when this component starts the trace. This keeps partial traces intact when the proxy sits mid-chain behind the Cloudflare edge.

## Log correlation

The proxy and controller wrap their `slog` handler so that whenever a request context carries an active span, emitted log lines include `trace_id` and `span_id`. This means an access-log line or an error log can be pivoted directly to its trace in the backend. No additional configuration is needed beyond enabling tracing.

## Known gap

WebSocket **backend** handshakes are not span-propagated. The WebSocket upgrade path dials the backend directly (bypassing the reverse-proxy transport that injects `traceparent`), so the backend handshake of a WebSocket route carries no client span and no propagated context. The server span still covers the request. This is a deliberate scope boundary, not a bug — the upgrade path's hijack contract is delicate and is left untouched.

## Cost when disabled

Zero on the hot path. When tracing is off, the proxy handler skips trace-context extraction, span creation, and the request-context rebuild entirely, and the backend transport is left unwrapped so an inbound `traceparent` is still forwarded to the backend byte-for-byte. The controller's outbound clients are likewise left unwrapped.

## Example collector endpoints

| Backend | Endpoint shape |
| --- | --- |
| OpenTelemetry Collector (in-cluster, plaintext) | `otel-collector.observability:4317` |
| Grafana Tempo (distributor, plaintext gRPC) | `tempo-distributor.tempo:4317` |
| Jaeger (OTLP gRPC receiver) | `jaeger-collector.tracing:4317` |
| TLS collector | `https://otel.example.com:4317` |
