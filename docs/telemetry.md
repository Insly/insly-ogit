# OpenTelemetry

ogit exports traces and operational metrics through OTLP. Configure an
OpenTelemetry Collector or another OTLP receiver before starting `server` or
`bootstrap`. There is no metrics HTTP endpoint in ogit.

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_SERVICE_NAME=ogit
export OTEL_RESOURCE_ATTRIBUTES='deployment.environment.name=example,cloud.region=example-region,service.instance.id=unique-process-id'
export OTEL_METRIC_EXPORT_INTERVAL=15000
export OTEL_TRACES_SAMPLER=parentbased_traceidratio
export OTEL_TRACES_SAMPLER_ARG=0.1
ogit server --config /config/config.yaml
```

Use a unique `service.instance.id` per process and retain instance identity in
metrics to avoid merging independent cumulative counters. The example endpoint
is appropriate for a trusted local collector; configure TLS and authentication
for remote transport using standard `OTEL_EXPORTER_OTLP_*` settings. Supply
headers through your secret manager. `service.name` defaults to `ogit`;
`service.version` is the binary build version. Resource attributes describe the
deployment, not repositories or individual requests.

Each signal is opt-in: a generic OTLP endpoint enables both; a signal-specific
endpoint or `OTEL_TRACES_EXPORTER=otlp` / `OTEL_METRICS_EXPORTER=otlp` enables
only that signal. Without an endpoint, an explicitly enabled exporter uses the
SDK's standard localhost endpoint. Exporter selectors accept `otlp` or `none`;
`none` disables that signal even with a shared endpoint. `OTEL_SDK_DISABLED=true`
disables both. Unconfigured local runs do not export telemetry.

Both `http/protobuf` and `grpc` are supported. Signal-specific HTTP endpoints
must include `/v1/traces` or `/v1/metrics`; the generic endpoint adds these paths.
The default protocol is the SDK's `http/protobuf`. Standard OTLP certificate,
header, compression, timeout and signal-specific settings apply. See the
[OTLP exporter configuration](https://opentelemetry.io/docs/specs/otel/protocol/exporter/)
and [Go exporter documentation](https://opentelemetry.io/docs/languages/go/exporters/).

Traces use the SDK batch processor; metrics use its periodic reader (60 seconds
by default). Export does not wait on the collector in request handlers. Graceful
shutdown gives both exporters a shared five-second deadline using a fresh context,
including for short-lived bootstrap commands. A collector outage can drop
telemetry but does not change a successful Git operation into a failure. Monitor
collector/exporter health and missing service telemetry separately. Invalid
exporter configuration can fail command startup.

## Traces and logs

HTTP server spans extract W3C `traceparent` and `tracestate` and retain the trace
through Git handling and S3 SDK calls. Application spans cover `git.receive_pack`,
`git.ref.publish`, `git.bootstrap` and each `git.replicate` attempt. Replication
attempts are independent root traces. HTTP request logs include `trace_id` and
`span_id` when a valid context is present. Existing structured application logs
remain on stdout; no OTLP log exporter is installed.

Application spans use route templates and bounded outcomes. They do not add
raw URLs, request headers, bodies, repository names, revisions or raw error text.
S3 spans use the AWS SDK's native instrumentation and may carry AWS operation
metadata; review collector redaction policy for your deployment. `/health` and
`/ready` record metrics without creating HTTP spans to avoid probe trace volume.
Trace sampling uses the standard SDK settings; the default is parent-based
always-on. Sampling traces does not sample metrics.

## Metrics to monitor

These are the only exported instrument names. Attributes are filtered before
aggregation to keep time-series counts bounded. Histogram units are seconds.
Counter outcomes are `success`, `conflict`, `rejected`, `error` or `canceled`,
where applicable. A canceled reconciliation during shutdown is not an outage.

| Instrument | Attributes | Operational use |
| --- | --- | --- |
| `http.server.request.duration` (histogram) | `http.request.method`, `http.route`, `http.response.status_code` | Request rate from count, 5xx ratio and p95/p99 latency. Split probes from Git routes. Investigate sustained 401/403 separately. |
| `ogit.git.pushes` (counter) | `ogit.outcome` | Actual push success/failure. Git can return HTTP 200 with a rejected update; HTTP error rates alone miss this. |
| `ogit.ref.publications` (counter) | `ogit.outcome`, `ogit.ref.operation` (`create`/`update`) | Final durable reference publication outcomes across pushes, bootstrap and replication, including initial symbolic HEAD creation. Conflicts indicate contention; errors indicate inability to publish. |
| `ogit.replication.duration` (histogram) | `ogit.outcome` | Attempt rate, failures and reconciliation latency. Repeated errors or duration approaching the attempt timeout need investigation. |
| `ogit.replication.last_success.age` (gauge, seconds) | none | Time since the controller last successfully validated the source and local branch; before first success, time since controller start. Detects unreachable sources and stalled reconciliation. |
| `ogit.replication.pending.age` (gauge, seconds) | none | Age of a known pending revision reported by a completed attempt. Detects sustained inability to apply observed updates. |
| `client.call.duration` (histogram) | `rpc.service`, `rpc.method`, `exception.type` when present | S3 operation latency and call rate from count. |
| `client.call.errors` (counter) | same SDK attributes | Diagnose S3 permission, availability and transport errors by operation/error type. |
| `client.call.attempts` (counter) | same SDK attributes | Compare attempt rate with call rate to detect retry amplification. |

Freshness gauges exist only in processes running a replication controller. They
keep aging during blocked I/O; metric collection never calls S3 or waits for the
reconciliation lock. State is process-local and resets on restart. A zero pending
age means no known pending revision, not proof of freshness: when the source is
unreachable the controller cannot discover new commits. Always use last-success
age alongside pending age, and alert on absent metrics/process availability.
Neither gauge measures commit-to-consumer propagation delay. Measure that at the
consuming application if an end-to-end freshness objective is required.

Start with an alert when last-success age exceeds the attempt timeout plus
several poll intervals for a sustained window; allow an explicit startup grace
period. Apply a separate pending-age threshold based on acceptable update delay.
Track error outcomes over a window rather than paging on a single retry or
conflict. S3 404s during initialization and 412s during conditional publication
can be expected: use final publication outcomes to determine user-visible
failure. Successful reconciliation after an ambiguous response counts as success.

For serving traffic, define latency and 5xx objectives from the consumer's needs,
then alert with a minimum request volume. Watch push `error` rates and persistent
`conflict`/`rejected` rates separately; the latter often need caller investigation.
Use platform monitoring for process availability, CPU, memory and restarts.

Backends may translate dots to underscores, append counter/unit suffixes and
map resource attributes to labels. Build queries against the names emitted by
your collector/backend, using histogram counts for rates and histogram buckets
for quantiles. Keep per-instance freshness visible; an average across healthy
and stale replicas can conceal the stale replica.
