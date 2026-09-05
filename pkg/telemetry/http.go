package telemetry

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// HTTP records route-level latency/status without capturing request URLs or contents.
func HTTP() fiber.Handler {
	tracer := otel.Tracer(Scope)
	duration, _ := otel.Meter(Scope).Float64Histogram("http.server.request.duration", metric.WithUnit("s"), metric.WithDescription("HTTP request latency including application processing"))
	return func(c fiber.Ctx) error {
		start := time.Now()
		method := c.Method()
		switch method {
		case "GET", "POST", "HEAD", "PUT", "DELETE", "PATCH", "OPTIONS", "CONNECT", "TRACE":
		default:
			method = "_OTHER"
		}
		route := requestRoute(c.Path())
		saved := c.Context()
		ctx := otel.GetTextMapPropagator().Extract(saved, propagation.MapCarrier{"traceparent": c.Get("traceparent"), "tracestate": c.Get("tracestate")})
		var span trace.Span
		if route != "/health" && route != "/ready" {
			ctx, span = tracer.Start(ctx, method+" "+route, trace.WithSpanKind(trace.SpanKindServer))
			defer span.End()
		}
		c.SetContext(ctx)
		defer c.SetContext(saved)
		err := c.Next()
		status := c.Response().StatusCode()
		if err != nil {
			status = 500
			var fiberErr *fiber.Error
			if errors.As(err, &fiberErr) {
				status = fiberErr.Code
			}
		}
		if matched := c.Route().Path; matched != "" && matched != "/" && matched != "/*" {
			route = matched
		}
		attrs := []attribute.KeyValue{attribute.String("http.request.method", method), attribute.String("http.route", route), attribute.Int("http.response.status_code", status)}
		duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		if span != nil {
			span.SetName(method + " " + route)
			span.SetAttributes(attrs...)
			if status >= 500 {
				span.SetStatus(codes.Error, "http.server.error")
			}
		}
		return err
	}
}
func requestRoute(path string) string {
	switch path {
	case "/health", "/ready", "/api/repo", "/api/repos", "/replication/status":
		return path
	}
	for _, suffix := range []string{"/info/refs", "/git-upload-pack", "/git-receive-pack"} {
		if strings.HasSuffix(path, suffix) {
			return "/:repo" + suffix
		}
	}
	return "unmatched"
}
