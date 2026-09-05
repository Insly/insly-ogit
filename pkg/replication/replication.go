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
	mu                  sync.Mutex
	status              Status
}

func (m *Mirror) Status() Status { m.mu.Lock(); defer m.mu.Unlock(); return m.status }
func scoped(s storer.Storer, ctx context.Context) storer.Storer {
	if s3, ok := s.(*s3store.S3Storer); ok {
		return s3.WithContext(ctx)
	}
	return s
}

// Reconcile needs no in-memory checkpoint. All progress is committed in the destination ref.
func (m *Mirror) Reconcile(ctx context.Context) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.LastAttempt = time.Now().UTC()
	defer func() {
		if err != nil {
			m.status.Error = err.Error()
		} else {
			m.status.Error = ""
			m.status.LastSuccess = time.Now().UTC()
			m.status.PendingSince = time.Time{}
		}
	}()
	if !m.Branch.IsBranch() {
		return fmt.Errorf("mirror requires an explicit branch")
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
	} else if err != nil {
		return err
	}
	if old != nil {
		m.status.AppliedRevision = old.Hash().String()
	}
	if m.status.SourceRevision != m.status.AppliedRevision && m.status.PendingSince.IsZero() {
		m.status.PendingSince = time.Now().UTC()
	}
	// Validate the complete source even when its head is unchanged, so missing or
	// corrupt source objects are visible. Transfer only objects absent locally.
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
	if old != nil && old.Hash() == source.Hash() {
		return nil
	}
	if err = dst.CheckAndSetReference(plumbing.NewHashReference(m.Branch, source.Hash()), old); err != nil {
		return err
	}
	m.status.AppliedRevision = source.Hash().String()
	return nil
}

// Run retries immediately on startup and then periodically, including after failures.
func (m *Mirror) Run(ctx context.Context, interval, timeout time.Duration, observe func(Status)) error {
	if interval <= 0 || timeout <= 0 {
		return fmt.Errorf("positive replication interval and timeout required")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt, cancel := context.WithTimeout(ctx, timeout)
		_ = m.Reconcile(attempt)
		cancel()
		if observe != nil {
			observe(m.Status())
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
