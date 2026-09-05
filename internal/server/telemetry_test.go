package server

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/labbs/git-server-s3/internal/api/controller"
	"github.com/labbs/git-server-s3/internal/config"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/labbs/git-server-s3/internal/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
)

func TestHTTPObservability(t *testing.T) {
	o := testutil.ObserveTelemetry(t)
	var logs bytes.Buffer
	cfg := HttpConfig{HttpLogs: true, Logger: zerolog.New(&logs)}
	cfg.Configure()
	cfg.Fiber.Get("/:repo/info/refs", func(c fiber.Ctx) error { return fiber.NewError(503, "SECRET-error") })
	const id = "0123456789abcdef0123456789abcdef"
	req := httptest.NewRequest("GET", "http://SECRET-host/SECRET-repo/info/refs?token=SECRET-query", nil)
	req.Header.Set("traceparent", "00-"+id+"-0123456789abcdef-01")
	req.Header.Set("Authorization", "Bearer SECRET-token")
	resp, err := cfg.Fiber.Test(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 503, resp.StatusCode)
	require.Equal(t, "SECRET-error", string(body))
	spans := o.Spans.GetSpans()
	require.Len(t, spans, 1, "HTTP request must emit a span")
	span := spans[0]
	require.Equal(t, id, span.SpanContext.TraceID().String())
	require.Equal(t, "0123456789abcdef", span.Parent.SpanID().String())
	require.Equal(t, trace.SpanKindServer, span.SpanKind)
	require.Equal(t, "GET /:repo/info/refs", span.Name)
	require.Equal(t, codes.Error, span.Status.Code)
	for _, a := range span.Attributes {
		require.NotContains(t, a.Value.AsString(), "SECRET")
	}
	require.NotContains(t, span.Status.Description, "SECRET")
	require.Empty(t, span.Events, "raw handler error text must not become telemetry")
	var entry map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
	require.Equal(t, id, entry["trace_id"])
	require.Equal(t, span.SpanContext.SpanID().String(), entry["span_id"])
	hist := o.Collect(t)["http.server.request.duration"].Data.(metricdata.Histogram[float64])
	require.Len(t, hist.DataPoints, 1)
	require.Equal(t, uint64(1), hist.DataPoints[0].Count)
	attrs := hist.DataPoints[0].Attributes
	route, _ := attrs.Value("http.route")
	require.Equal(t, "/:repo/info/refs", route.AsString())
	status, _ := attrs.Value("http.response.status_code")
	require.Equal(t, int64(503), status.AsInt64())
	require.Equal(t, 3, attrs.Len())
}
func TestProbeMetricsWithoutProbeSpans(t *testing.T) {
	o := testutil.ObserveTelemetry(t)
	cfg := HttpConfig{Logger: zerolog.Nop()}
	cfg.Configure()
	for _, path := range []string{"/health", "/ready"} {
		resp, err := cfg.Fiber.Test(httptest.NewRequest("GET", path, nil))
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
	}
	require.Empty(t, o.Spans.GetSpans())
	m := o.Collect(t)
	require.Contains(t, m, "http.server.request.duration", "probes must still expose readiness status")
}

func TestRejectedPushIsVisibleDespiteHTTP200(t *testing.T) {
	f := testutil.NewS3(t)
	previous := config.Storage
	t.Cleanup(func() { config.Storage = previous })
	config.Storage.Type = "s3"
	config.Storage.S3.Bucket = "b"
	config.Storage.S3.Region = "us-east-1"
	config.Storage.S3.Endpoint = f.Server.URL
	config.Storage.S3.AccessKey = "test"
	config.Storage.S3.SecretKey = "test"
	store := s3store.NewS3Storage(zerolog.Nop())
	require.NoError(t, store.Configure())
	head, err := store.Bootstrap(context.Background(), "private", "main", map[string][]byte{"flags.yaml": []byte("flag: true")})
	require.NoError(t, err)
	o := testutil.ObserveTelemetry(t)
	cfg := HttpConfig{Logger: zerolog.Nop()}
	cfg.Configure()
	controller := controller.GitController{Storage: store, Logger: zerolog.Nop()}
	cfg.Fiber.Post("/:repo/git-receive-pack", controller.HandleReceivePack)
	req := packp.NewReferenceUpdateRequest()
	require.NoError(t, req.Capabilities.Set(capability.ReportStatus))
	req.Commands = []*packp.Command{{Name: "refs/heads/main", Old: head, New: plumbing.ZeroHash}}
	var body bytes.Buffer
	require.NoError(t, req.Encode(&body))
	response, err := cfg.Fiber.Test(httptest.NewRequest("POST", "/private.git/git-receive-pack", &body))
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, 200, response.StatusCode)
	var report packp.ReportStatus
	require.NoError(t, report.Decode(response.Body))
	require.Error(t, report.Error(), "wire response must reject the deletion")
	counters := o.Collect(t)["ogit.git.pushes"].Data.(metricdata.Sum[int64]).DataPoints
	require.Len(t, counters, 1)
	outcome, _ := counters[0].Attributes.Value("ogit.outcome")
	require.Equal(t, "rejected", outcome.AsString())
	require.Equal(t, int64(1), counters[0].Value)
	var failedChild bool
	for _, span := range o.Spans.GetSpans() {
		if span.Name == "git.receive_pack" {
			failedChild = span.Status.Code == codes.Error
		}
	}
	require.True(t, failedChild, "Git rejection needs an error span even when HTTP succeeds")
	current, err := store.StorerForRepository("private").Reference("refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, head, current.Hash())
}
