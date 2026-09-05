package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// Freshness is independent of the replication/storage locks. Collection only reads
// this snapshot, so an unavailable source cannot stall metric export.
type Freshness struct {
	mu                            sync.Mutex
	started, lastSuccess, pending time.Time
	registration                  metric.Registration
	once                          sync.Once
}

func WatchReplication() *Freshness { return watchReplication(time.Now) }
func watchReplication(now func() time.Time) *Freshness {
	f := &Freshness{started: now()}
	meter := otel.Meter(Scope)
	success, err := meter.Float64ObservableGauge("ogit.replication.last_success.age", metric.WithUnit("s"), metric.WithDescription("Seconds since successful source validation, or controller start before first success"))
	if err != nil {
		otel.Handle(err)
		return f
	}
	pending, err := meter.Float64ObservableGauge("ogit.replication.pending.age", metric.WithUnit("s"), metric.WithDescription("Age of the known pending revision observed by a completed attempt"))
	if err != nil {
		otel.Handle(err)
		return f
	}
	f.registration, err = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		f.mu.Lock()
		last, pendingSince := f.lastSuccess, f.pending
		if last.IsZero() {
			last = f.started
		}
		f.mu.Unlock()
		current := now()
		observer.ObserveFloat64(success, max(0, current.Sub(last).Seconds()))
		age := float64(0)
		if !pendingSince.IsZero() {
			age = max(0, current.Sub(pendingSince).Seconds())
		}
		observer.ObserveFloat64(pending, age)
		return nil
	}, success, pending)
	if err != nil {
		otel.Handle(err)
	}
	return f
}
func (f *Freshness) Update(lastSuccess, pending time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSuccess = lastSuccess
	f.pending = pending
}
func (f *Freshness) Close() {
	f.once.Do(func() {
		if f.registration != nil {
			if err := f.registration.Unregister(); err != nil {
				otel.Handle(err)
			}
		}
	})
}
