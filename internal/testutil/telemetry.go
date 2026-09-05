package testutil

import (
	"context"
	"testing"

	"github.com/labbs/git-server-s3/pkg/telemetry"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type Telemetry struct {
	Reader *sdkmetric.ManualReader
	Spans  *tracetest.InMemoryExporter
}

func ObserveTelemetry(t testing.TB) *Telemetry {
	t.Helper()
	tp, mp, prop := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	o := &Telemetry{Reader: sdkmetric.NewManualReader(), Spans: tracetest.NewInMemoryExporter()}
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSyncer(o.Spans))
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(o.Reader), sdkmetric.WithView(telemetry.MetricView))
	otel.SetTracerProvider(tracer)
	otel.SetMeterProvider(meter)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tracer.Shutdown(context.Background())
		_ = meter.Shutdown(context.Background())
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
	})
	return o
}
func (o *Telemetry) Collect(t testing.TB) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, o.Reader.Collect(context.Background(), &rm))
	result := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			result[m.Name] = m
		}
	}
	return result
}
