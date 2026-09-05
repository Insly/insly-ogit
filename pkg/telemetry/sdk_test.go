package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"OTEL_SDK_DISABLED", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_SERVICE_NAME", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"} {
		t.Setenv(key, "")
	}
	t.Setenv("OTEL_TRACES_SAMPLER", "parentbased_always_on")
	tp, mp, prop := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(tp); otel.SetMeterProvider(mp); otel.SetTextMapPropagator(prop) })
}

func TestOTLPExportAndDisable(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "disabled"}[disabled], func(t *testing.T) {
			cleanEnv(t)
			var mu sync.Mutex
			var traces tracepb.ExportTraceServiceRequest
			var metrics metricpb.ExportMetricsServiceRequest
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/v1/traces":
					err = proto.Unmarshal(b, &traces)
				case "/v1/metrics":
					err = proto.Unmarshal(b, &metrics)
				default:
					t.Errorf("unexpected OTLP path %s", r.URL.Path)
				}
				if err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer receiver.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
			t.Setenv("OTEL_SERVICE_NAME", "ogit-test")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=test,cloud.region=test-region")
			if disabled {
				t.Setenv("OTEL_SDK_DISABLED", "true")
			}
			runtime, err := Setup(context.Background(), "test-version")
			require.NoError(t, err)
			ctx, span := otel.Tracer("test").Start(context.Background(), "test.operation")
			span.End()
			h, err := otel.Meter("test").Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
			require.NoError(t, err)
			h.Record(ctx, 0.25, metric.WithAttributes(attribute.String("http.route", "/:repo/info/refs"), attribute.String("repository", "secret-repo")))
			junk, err := otel.Meter("test").Int64Counter("unmonitored.instrument")
			require.NoError(t, err)
			junk.Add(ctx, 1)
			end, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			require.NoError(t, runtime.Shutdown(end))
			mu.Lock()
			defer mu.Unlock()
			if disabled {
				require.Empty(t, traces.ResourceSpans)
				require.Empty(t, metrics.ResourceMetrics)
				return
			}
			require.NotEmpty(t, traces.ResourceSpans, "shutdown must export completed spans")
			require.NotEmpty(t, metrics.ResourceMetrics, "shutdown must export accumulated metrics")
			resource := map[string]string{}
			for _, a := range traces.ResourceSpans[0].Resource.Attributes {
				resource[a.Key] = a.Value.GetStringValue()
			}
			require.Equal(t, "ogit-test", resource["service.name"])
			require.Equal(t, "test-version", resource["service.version"])
			require.Equal(t, "test", resource["deployment.environment.name"])
			var found bool
			for _, rm := range metrics.ResourceMetrics {
				for _, sm := range rm.ScopeMetrics {
					for _, m := range sm.Metrics {
						require.NotEqual(t, "unmonitored.instrument", m.Name)
						if m.Name == "http.server.request.duration" {
							found = true
							require.Equal(t, "s", m.Unit)
							points := m.GetHistogram().DataPoints
							require.Len(t, points, 1)
							require.Equal(t, uint64(1), points[0].Count)
							require.InDelta(t, 0.25, points[0].GetSum(), 0.0001)
							for _, a := range points[0].Attributes {
								require.NotEqual(t, "repository", a.Key)
							}
						}
					}
				}
			}
			require.True(t, found, "selected request latency histogram must be exported")
		})
	}
}
