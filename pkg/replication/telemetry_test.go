package replication

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/labbs/git-server-s3/internal/testutil"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestFreshnessCollectionWhileReplicationIsBlocked(t *testing.T) {
	o := testutil.ObserveTelemetry(t)
	f := testutil.NewS3(t)
	src := s3store.NewS3Storer(f.Client(), "source", "repo.git", zerolog.Nop())
	dst := s3store.NewS3Storer(f.Client(), "replica", "repo.git", zerolog.Nop())
	head := commit(t, src, plumbing.ZeroHash, "initial")
	require.NoError(t, src.SetReference(plumbing.NewHashReference(branch, head)))
	var reads atomic.Int32
	blocked, release := make(chan struct{}), make(chan struct{})
	f.Before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "GET" && r.URL.Path == "/source/repo.git/refs/heads/main" {
			switch reads.Add(1) {
			case 1:
				time.Sleep(200 * time.Millisecond)
			case 2:
				close(blocked)
				<-release
			}
		}
		return false
	}
	mirror := Mirror{Source: src, Destination: dst, Branch: branch}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	observed := make(chan Status, 2)
	go func() { done <- mirror.Run(ctx, time.Millisecond, time.Minute, func(s Status) { observed <- s }) }()
	defer func() { cancel(); close(release); <-done }()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("second reconciliation did not block")
	}
	collect := func() float64 {
		var rm metricdata.ResourceMetrics
		result := make(chan error, 1)
		go func() { result <- o.Reader.Collect(context.Background(), &rm) }()
		select {
		case err := <-result:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("metrics blocked behind reconciliation")
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name == "ogit.replication.last_success.age" {
					return m.Data.(metricdata.Gauge[float64]).DataPoints[0].Value
				}
			}
		}
		t.Fatal("missing live freshness metric")
		return 0
	}
	lastSuccess := (<-observed).LastSuccess
	require.False(t, lastSuccess.IsZero())
	first := collect()
	require.LessOrEqual(t, first, time.Since(lastSuccess).Seconds()+0.05, "successful work must reset the initial startup age")
	time.Sleep(20 * time.Millisecond)
	require.Greater(t, collect(), first, "stalled work must keep aging in telemetry")
}
