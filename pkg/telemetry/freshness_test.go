package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestReplicationFreshness(t *testing.T) {
	previous := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(MetricView))
	otel.SetMeterProvider(provider)
	defer func() { _ = provider.Shutdown(context.Background()); otel.SetMeterProvider(previous) }()
	now := time.Unix(100, 0)
	watch := watchReplication(func() time.Time { return now })
	defer watch.Close()
	collect := func() map[string]float64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		values := map[string]float64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				data := m.Data.(metricdata.Gauge[float64])
				for _, p := range data.DataPoints {
					require.Zero(t, p.Attributes.Len())
					values[m.Name] = p.Value
				}
			}
		}
		return values
	}
	now = now.Add(30 * time.Second)
	require.Equal(t, map[string]float64{"ogit.replication.last_success.age": 30, "ogit.replication.pending.age": 0}, collect(), "never-successful controllers must age from startup")
	watch.Update(now, time.Time{})
	now = now.Add(10 * time.Second)
	require.Equal(t, float64(10), collect()["ogit.replication.last_success.age"])
	watch.Update(now.Add(-10*time.Second), now.Add(-5*time.Second))
	now = now.Add(20 * time.Second)
	require.Equal(t, map[string]float64{"ogit.replication.last_success.age": 30, "ogit.replication.pending.age": 25}, collect(), "freshness must age without another reconciliation")
	watch.Update(now, time.Time{})
	require.Equal(t, map[string]float64{"ogit.replication.last_success.age": 0, "ogit.replication.pending.age": 0}, collect())
	watch.Close()
	require.Empty(t, collect(), "stopped controllers must unregister callbacks")
}
