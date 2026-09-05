package telemetry

import (
	"context"
	"errors"
	"time"

	gitstorage "github.com/go-git/go-git/v5/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// StartOperation deliberately records bounded outcomes instead of raw error text.
func StartOperation(ctx context.Context, name string) (context.Context, func(string)) {
	ctx, span := otel.Tracer(Scope).Start(ctx, name)
	return ctx, func(outcome string) {
		span.SetAttributes(attribute.String("ogit.outcome", outcome))
		if outcome != "success" {
			span.SetStatus(codes.Error, outcome)
		}
		span.End()
	}
}
func Outcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, gitstorage.ErrReferenceHasChanged):
		return "conflict"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}
func Count(ctx context.Context, name, outcome string, extra ...attribute.KeyValue) {
	counter, _ := otel.Meter(Scope).Int64Counter(name, metric.WithUnit("{operation}"))
	attrs := append([]attribute.KeyValue{attribute.String("ogit.outcome", outcome)}, extra...)
	counter.Add(ctx, 1, metric.WithAttributes(attrs...))
}
func Duration(ctx context.Context, name, outcome string, start time.Time) {
	histogram, _ := otel.Meter(Scope).Float64Histogram(name, metric.WithUnit("s"))
	histogram.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("ogit.outcome", outcome)))
}
