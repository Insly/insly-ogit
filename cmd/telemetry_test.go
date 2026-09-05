package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	metricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	spanpb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// TestProcessTelemetryExport exercises OTLP from the actual CLI, including
// short-lived bootstrap and graceful server termination after a native Git push.
func TestProcessTelemetryExport(t *testing.T) {
	var mu sync.Mutex
	spans := map[string]*spanpb.Span{}
	metrics := map[string]bool{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/traces":
			var req tracepb.ExportTraceServiceRequest
			if err := proto.Unmarshal(body, &req); err != nil {
				t.Error(err)
				return
			}
			for _, rs := range req.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, s := range ss.Spans {
						spans[string(s.SpanId)] = s
					}
				}
			}
		case "/v1/metrics":
			var req metricpb.ExportMetricsServiceRequest
			if err := proto.Unmarshal(body, &req); err != nil {
				t.Error(err)
				return
			}
			for _, rm := range req.ResourceMetrics {
				for _, sm := range rm.ScopeMetrics {
					for _, m := range sm.Metrics {
						metrics[m.Name] = true
					}
				}
			}
		default:
			t.Errorf("unexpected OTLP path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer receiver.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_always_on")
	// Traces require shutdown to flush; live replication gauges export periodically.
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "60000")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "100")
	require.True(t, t.Run("processes", func(t *testing.T) { runHAProcessIntegration(t, "") }))
	mu.Lock()
	defer mu.Unlock()
	names := map[string]bool{}
	var push *spanpb.Span
	for _, s := range spans {
		names[s.Name] = true
		if s.Name == "git.receive_pack" {
			push = s
		}
	}
	for _, name := range []string{"git.bootstrap", "git.receive_pack", "git.ref.publish", "git.replicate"} {
		require.True(t, names[name], "missing exported span %s", name)
	}
	require.NotNil(t, push)
	parent := spans[string(push.ParentSpanId)]
	require.NotNil(t, parent, "Git push must be nested under its HTTP request")
	require.Equal(t, spanpb.Span_SPAN_KIND_SERVER, parent.Kind)
	require.Equal(t, "POST /:repo/git-receive-pack", parent.Name)
	require.Equal(t, parent.TraceId, push.TraceId)
	var awsChild bool
	for _, s := range spans {
		if string(s.TraceId) == string(push.TraceId) && s.Kind == spanpb.Span_SPAN_KIND_CLIENT {
			awsChild = true
		}
	}
	require.True(t, awsChild, "S3 calls must stay within the originating HTTP trace")
	for _, name := range []string{"http.server.request.duration", "ogit.git.pushes", "ogit.ref.publications", "ogit.replication.duration", "ogit.replication.last_success.age", "ogit.replication.pending.age", "client.call.duration", "client.call.attempts"} {
		require.True(t, metrics[name], "missing exported metric %s", name)
	}
}
