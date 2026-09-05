package zerolog

import (
	"slices"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	z "github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
)

func HTTPLogger(logger z.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		timeStart := time.Now()
		err := c.Next()
		var _logger *z.Event
		if c.Response().StatusCode() >= 399 {
			_logger = logger.Error()
		} else {
			_logger = logger.Info()
		}

		if slices.Contains([]string{"/health", "/metrics", "/favicon.ico"}, c.Path()) {
			return err
		}

		if sc := trace.SpanContextFromContext(c.Context()); sc.IsValid() {
			_logger.Str("trace_id", sc.TraceID().String()).Str("span_id", sc.SpanID().String())
		}

		_logger.
			Int("status", c.Response().StatusCode()).
			Dur("duration", time.Since(timeStart)).
			Str("method", string(c.Request().Header.Method())).
			Str("remote_addr", c.IP()).
			Str("path", c.Path()).
			Str("user_agent", c.Get("User-Agent")).
			Int("bytes_sent", c.Response().Header.ContentLength()).
			Int("bytes_received", c.Request().Header.ContentLength()).
			Str("proto", c.Scheme()).
			Str("host", c.Host()).
			Str("request_id", requestid.FromContext(c)).
			Send()
		return err
	}
}
