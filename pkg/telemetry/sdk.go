// Package telemetry provides OpenTelemetry export and bounded operational signals.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const Scope = "github.com/labbs/git-server-s3/otel"

type Runtime struct {
	trace *sdktrace.TracerProvider
	meter *sdkmetric.MeterProvider
	once  sync.Once
	err   error
}

// Setup installs process-wide providers. Call before constructing servers or SDK clients.
// Each signal is opt-in through its standard endpoint or exporter environment settings.
func Setup(ctx context.Context, version string) (*Runtime, error) {
	r := &Runtime{}
	disabled := false
	if value := os.Getenv("OTEL_SDK_DISABLED"); value != "" {
		var err error
		disabled, err = strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("invalid OTEL_SDK_DISABLED")
		}
	}
	if !disabled {
		res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", "ogit"), attribute.String("service.version", version)), resource.WithFromEnv(), resource.WithTelemetrySDK())
		if err != nil {
			return nil, err
		}
		if configured("TRACES") {
			if err := validateExporter("TRACES"); err != nil {
				return nil, err
			}
			exporter, err := autoexport.NewSpanExporter(ctx)
			if err != nil {
				return nil, err
			}
			if !autoexport.IsNoneSpanExporter(exporter) {
				r.trace = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exporter))
			}
		}
		if configured("METRICS") {
			if err := validateExporter("METRICS"); err != nil {
				_ = r.Shutdown(ctx)
				return nil, err
			}
			reader, err := autoexport.NewMetricReader(ctx)
			if err != nil {
				_ = r.Shutdown(ctx)
				return nil, err
			}
			if !autoexport.IsNoneMetricReader(reader) {
				r.meter = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader), sdkmetric.WithView(MetricView))
			}
		}
	}
	if r.trace != nil {
		otel.SetTracerProvider(r.trace)
	} else {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
	}
	if r.meter != nil {
		otel.SetMeterProvider(r.meter)
	} else {
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
	}
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return r, nil
}
func configured(signal string) bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != "" || os.Getenv("OTEL_"+signal+"_EXPORTER") != ""
}
func validateExporter(signal string) error {
	switch os.Getenv("OTEL_" + signal + "_EXPORTER") {
	case "", "otlp", "none":
		return nil
	default:
		return fmt.Errorf("OTEL_%s_EXPORTER must be otlp or none", signal)
	}
}

// Shutdown flushes both signals concurrently using the caller's bounded context.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.once.Do(func() {
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		if r.trace != nil {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- r.trace.Shutdown(ctx) }()
		}
		if r.meter != nil {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- r.meter.Shutdown(ctx) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			r.err = errors.Join(r.err, err)
		}
	})
	return r.err
}

// MetricView exports only signals used by the service's monitoring guide.
// Filtering before aggregation prevents per-repository/URL/request series growth.
func MetricView(i sdkmetric.Instrument) (sdkmetric.Stream, bool) {
	var keys []attribute.Key
	switch i.Name {
	case "http.server.request.duration":
		keys = []attribute.Key{"http.request.method", "http.route", "http.response.status_code"}
	case "client.call.duration", "client.call.errors", "client.call.attempts":
		keys = []attribute.Key{"rpc.service", "rpc.method", "exception.type"}
	case "ogit.git.pushes", "ogit.replication.duration":
		keys = []attribute.Key{"ogit.outcome"}
	case "ogit.ref.publications":
		keys = []attribute.Key{"ogit.outcome", "ogit.ref.operation"}
	case "ogit.replication.last_success.age", "ogit.replication.pending.age":
		keys = []attribute.Key{}
	default:
		return sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}}, true
	}
	stream := sdkmetric.Stream{Name: i.Name, Description: i.Description, Unit: i.Unit, AttributeFilter: attribute.NewAllowKeysFilter(keys...)}
	if i.Kind == sdkmetric.InstrumentKindHistogram {
		stream.Aggregation = sdkmetric.AggregationExplicitBucketHistogram{Boundaries: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}}
	}
	return stream, true
}
