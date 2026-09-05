package s3

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/labbs/git-server-s3/internal/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestS3TelemetryAndScopedContext(t *testing.T) {
	o := testutil.ObserveTelemetry(t)
	f := testutil.NewS3(t)
	client, err := NewClient(context.Background(), "us-east-1", f.Server.URL, "test", "test", "")
	require.NoError(t, err)
	store := &S3Storage{client: client, bucket: "b", Logger: zerolog.Nop()}
	ctx, parent := otel.Tracer("test").Start(context.Background(), "request")
	scoped := store.WithContext(ctx)
	require.NoError(t, scoped.CreateRepository("private.git"))
	st, err := scoped.GetStorer("private.git")
	require.NoError(t, err)
	_, err = st.Reference("refs/heads/main")
	require.NoError(t, err)
	parent.End()
	spans := o.Spans.GetSpans()
	var awsSpans int
	for _, s := range spans {
		if strings.Contains(s.Name, "GetObject") || strings.Contains(s.Name, "PutObject") || strings.Contains(s.Name, "HeadObject") {
			awsSpans++
			require.Equal(t, parent.SpanContext().TraceID(), s.SpanContext.TraceID(), "all repository/storer calls must retain request context")
		}
	}
	require.Greater(t, awsSpans, 2, "real SDK reads and writes must produce spans")
	before := len(spans)
	require.True(t, store.RepositoryExists("private.git"))
	for _, s := range o.Spans.GetSpans()[before:] {
		require.NotEqual(t, parent.SpanContext().TraceID(), s.SpanContext.TraceID(), "scoping must not mutate the shared store")
	}
	f.Before = func(w http.ResponseWriter, r *http.Request) bool { w.WriteHeader(403); return true }
	require.False(t, scoped.RepositoryExists("private.git"))
	metrics := o.Collect(t)
	for _, name := range []string{"client.call.duration", "client.call.errors", "client.call.attempts"} {
		require.Contains(t, metrics, name, "instrument real SDK operations")
	}
	for name, m := range metrics {
		if strings.HasPrefix(name, "client.") {
			require.Contains(t, []string{"client.call.duration", "client.call.errors", "client.call.attempts"}, name)
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					for _, a := range p.Attributes.ToSlice() {
						require.Contains(t, []string{"rpc.service", "rpc.method", "exception.type"}, string(a.Key))
					}
				}
			}
		}
	}
}
