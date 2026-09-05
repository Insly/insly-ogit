package telemetry

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	metricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

type traceReceiver struct {
	tracepb.UnimplementedTraceServiceServer
	count atomic.Int64
}

func (s *traceReceiver) Export(_ context.Context, req *tracepb.ExportTraceServiceRequest) (*tracepb.ExportTraceServiceResponse, error) {
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			s.count.Add(int64(len(ss.Spans)))
		}
	}
	return &tracepb.ExportTraceServiceResponse{}, nil
}

type metricReceiver struct {
	metricpb.UnimplementedMetricsServiceServer
	count atomic.Int64
}

func (s *metricReceiver) Export(_ context.Context, req *metricpb.ExportMetricsServiceRequest) (*metricpb.ExportMetricsServiceResponse, error) {
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			s.count.Add(int64(len(sm.Metrics)))
		}
	}
	return &metricpb.ExportMetricsServiceResponse{}, nil
}

func TestGRPCExportAndSampling(t *testing.T) {
	for _, sampler := range []string{"always_on", "always_off"} {
		t.Run(sampler, func(t *testing.T) {
			cleanEnv(t)
			l, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			server := grpc.NewServer()
			defer server.Stop()
			traces, metrics := &traceReceiver{}, &metricReceiver{}
			tracepb.RegisterTraceServiceServer(server, traces)
			metricpb.RegisterMetricsServiceServer(server, metrics)
			go server.Serve(l)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+l.Addr().String())
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
			t.Setenv("OTEL_TRACES_SAMPLER", sampler)
			runtime, err := Setup(context.Background(), "test")
			require.NoError(t, err)
			ctx, span := otel.Tracer("test").Start(context.Background(), "operation")
			Count(ctx, "ogit.git.pushes", "success")
			span.End()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			require.NoError(t, runtime.Shutdown(ctx))
			require.Positive(t, metrics.count.Load(), "metrics must survive trace sampling")
			if sampler == "always_on" {
				require.Equal(t, int64(1), traces.count.Load())
			} else {
				require.Zero(t, traces.count.Load())
			}
		})
	}
}

func TestSignalsAreOptIn(t *testing.T) {
	for _, mode := range []string{"default", "metrics-only", "traces-only", "explicit-none"} {
		t.Run(mode, func(t *testing.T) {
			cleanEnv(t)
			var traces, metrics atomic.Int32
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/traces" {
					traces.Add(1)
				} else if r.URL.Path == "/v1/metrics" {
					metrics.Add(1)
				} else {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer receiver.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
			switch mode {
			case "metrics-only":
				t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", receiver.URL+"/v1/metrics")
			case "traces-only":
				t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", receiver.URL+"/v1/traces")
			case "explicit-none":
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
				t.Setenv("OTEL_TRACES_EXPORTER", "none")
				t.Setenv("OTEL_METRICS_EXPORTER", "none")
			}
			runtime, err := Setup(context.Background(), "test")
			require.NoError(t, err)
			ctx, span := otel.Tracer("test").Start(context.Background(), "operation")
			Count(ctx, "ogit.git.pushes", "success")
			span.End()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.NoError(t, runtime.Shutdown(ctx))
			require.Equal(t, mode == "traces-only", traces.Load() > 0)
			require.Equal(t, mode == "metrics-only", metrics.Load() > 0)
			if mode == "default" || mode == "explicit-none" {
				require.False(t, span.SpanContext().IsValid())
			}
		})
	}
}

func TestUnavailableCollectorDoesNotBlockWorkOrShutdown(t *testing.T) {
	cleanEnv(t)
	received := make(chan struct{}, 10)
	release := make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer receiver.Close()
	defer close(release)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "1")
	runtime, err := Setup(context.Background(), "test")
	require.NoError(t, err)
	_, span := otel.Tracer("test").Start(context.Background(), "first")
	span.End()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("exporter never reached collector")
	}
	start := time.Now()
	for range 100 {
		ctx, span := otel.Tracer("test").Start(context.Background(), "work")
		Count(ctx, "ogit.git.pushes", "success")
		span.End()
	}
	require.Less(t, time.Since(start), time.Second, "collector must not be on the request path")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	require.Error(t, runtime.Shutdown(ctx))
	require.Less(t, time.Since(start), time.Second, "exporter outage must respect the shutdown deadline")
}
