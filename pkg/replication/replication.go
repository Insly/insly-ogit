// Package replication publishes complete, forward-only regional branch snapshots.
package replication

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/labbs/git-server-s3/pkg/gitgraph"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
	"github.com/labbs/git-server-s3/pkg/telemetry"
)

type Status struct {
	SourceRevision  string    `json:"source_revision"`
	AppliedRevision string    `json:"applied_revision"`
	LastAttempt     time.Time `json:"last_attempt"`
	LastSuccess     time.Time `json:"last_success"`
	PendingSince    time.Time `json:"pending_since,omitempty"`
	Error           string    `json:"error,omitempty"`
}
type Mirror struct {
	Source, Destination storer.Storer
	Branch              plumbing.ReferenceName
	// AuditInterval bounds how long unchanged objects may go without validation.
	// Zero uses DefaultAuditInterval. Configure before starting the controller.
	AuditInterval     time.Duration
	mu                sync.Mutex
	status            Status
	validatedRevision plumbing.Hash
	validatedAt       time.Time
}

const DefaultAuditInterval = time.Hour

func (m *Mirror) Status() Status { m.mu.Lock(); defer m.mu.Unlock(); return m.status }
func scoped(s storer.Storer, ctx context.Context) storer.Storer {
	if s3, ok := s.(*s3store.S3Storer); ok {
		return s3.WithContext(ctx)
	}
	return s
}

// Reconcile resumes from durable refs; its checkpoint only avoids repeated audits.
func (m *Mirror) Reconcile(ctx context.Context) (err error) {
	start := time.Now()
	ctx, end := telemetry.StartOperation(ctx, "git.replicate")
	defer func() {
		outcome := telemetry.Outcome(err)
		telemetry.Duration(ctx, "ogit.replication.duration", outcome, start)
		end(outcome)
	}()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.LastAttempt = time.Now().UTC()
	defer func() {
		if err != nil {
			m.status.Error = err.Error()
			m.validatedRevision = plumbing.ZeroHash
		} else {
			m.status.Error = ""
			m.status.LastSuccess = time.Now().UTC()
			m.status.PendingSince = time.Time{}
		}
	}()
	if !m.Branch.IsBranch() {
		return fmt.Errorf("mirror requires an explicit branch")
	}
	if m.AuditInterval < 0 {
		return fmt.Errorf("replication audit interval must not be negative")
	}
	if err = m.Branch.Validate(); err != nil {
		return err
	}
	src, dst := scoped(m.Source, ctx), scoped(m.Destination, ctx)
	source, err := src.Reference(m.Branch)
	if err != nil {
		return err
	}
	if source.Type() != plumbing.HashReference || source.Hash().IsZero() {
		return fmt.Errorf("source branch must name a commit")
	}
	m.status.SourceRevision = source.Hash().String()
	old, err := dst.Reference(m.Branch)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		old = nil
		m.status.AppliedRevision = ""
	} else if err != nil {
		return err
	}
	if old != nil {
		m.status.AppliedRevision = old.Hash().String()
	}
	if m.status.SourceRevision != m.status.AppliedRevision && m.status.PendingSince.IsZero() {
		m.status.PendingSince = time.Now().UTC()
	}
	auditInterval := m.AuditInterval
	if auditInterval == 0 {
		auditInterval = DefaultAuditInterval
	}
	// Equal refs alone do not prove completeness after restart or a failed audit.
	// Re-read both refs so destination loss or another publisher cannot be hidden.
	if old != nil && old.Type() == plumbing.HashReference && old.Hash() == source.Hash() &&
		m.validatedRevision == source.Hash() && time.Since(m.validatedAt) < auditInterval {
		return nil
	}
	// Validate on changed refs and periodic audits, transferring absent objects.
	ancestors, err := gitgraph.Walk(ctx, src, source.Hash(), func(obj plumbing.EncodedObject) error {
		existing, err := dst.EncodedObject(obj.Type(), obj.Hash())
		if err == nil {
			if existing.Type() != obj.Type() || existing.Hash() != obj.Hash() {
				return fmt.Errorf("corrupt destination object %s", obj.Hash())
			}
			return nil
		}
		if !errors.Is(err, plumbing.ErrObjectNotFound) {
			return err
		}
		hash, err := dst.SetEncodedObject(obj)
		if err != nil {
			return err
		}
		if hash != obj.Hash() {
			return fmt.Errorf("copied object hash mismatch")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if old != nil && (old.Type() != plumbing.HashReference || !ancestors[old.Hash()]) {
		return fmt.Errorf("refusing non-fast-forward regional publication")
	}
	if _, err = gitgraph.Walk(ctx, dst, source.Hash(), nil); err != nil {
		return fmt.Errorf("destination validation: %w", err)
	}
	// Metadata is initialization-only and never overwrites existing values.
	if s3, ok := dst.(*s3store.S3Storer); ok {
		if err = s3.EnsureRepository(m.Branch); err != nil {
			return err
		}
	}
	if old == nil || old.Hash() != source.Hash() {
		if err = dst.CheckAndSetReference(plumbing.NewHashReference(m.Branch, source.Hash()), old); err != nil {
			return err
		}
	}
	m.status.AppliedRevision = source.Hash().String()
	m.validatedRevision = source.Hash()
	m.validatedAt = time.Now()
	return nil
}

// Run retries immediately on startup and then periodically, including after failures.
func (m *Mirror) Run(ctx context.Context, interval, timeout time.Duration, observe func(Status)) error {
	if interval <= 0 || timeout <= 0 {
		return fmt.Errorf("positive replication interval and timeout required")
	}
	freshness := telemetry.WatchReplication()
	defer freshness.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt, cancel := context.WithTimeout(ctx, timeout)
		_ = m.Reconcile(attempt)
		cancel()
		status := m.Status()
		freshness.Update(status.LastSuccess, status.PendingSince)
		if observe != nil {
			observe(status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Ready checks only the published regional graph; upstream health is irrelevant.
func (m *Mirror) Ready(ctx context.Context) error {
	dst := scoped(m.Destination, ctx)
	ref, err := dst.Reference(m.Branch)
	if err != nil {
		return err
	}
	if ref.Type() != plumbing.HashReference {
		return fmt.Errorf("published branch must name a commit")
	}
	_, err = gitgraph.Walk(ctx, dst, ref.Hash(), nil)
	return err
}
