package replication

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/labbs/git-server-s3/internal/testutil"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

const branch plumbing.ReferenceName = "refs/heads/main"

func commit(t *testing.T, s storer.Storer, parent plumbing.Hash, content string) plumbing.Hash {
	t.Helper()
	blob := s.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, _ := blob.Writer()
	_, _ = w.Write([]byte(content))
	require.NoError(t, w.Close())
	bh, err := s.SetEncodedObject(blob)
	require.NoError(t, err)
	tree := object.Tree{Entries: []object.TreeEntry{{Name: "flags.yaml", Mode: 0100644, Hash: bh}}}
	enc := s.NewEncodedObject()
	require.NoError(t, tree.Encode(enc))
	th, err := s.SetEncodedObject(enc)
	require.NoError(t, err)
	c := object.Commit{TreeHash: th, Message: content, Author: object.Signature{Name: "EU", Email: "test@example.com", When: time.Unix(1, 0)}}
	c.Committer = c.Author
	if !parent.IsZero() {
		c.ParentHashes = []plumbing.Hash{parent}
	}
	enc = s.NewEncodedObject()
	require.NoError(t, c.Encode(enc))
	h, err := s.SetEncodedObject(enc)
	require.NoError(t, err)
	return h
}
func TestReplicationCompleteForwardOnlyAndRecovery(t *testing.T) {
	ctx := context.Background()
	f := testutil.NewS3(t)
	src := s3store.NewS3Storer(f.Client(), "eu", "repo.git", zerolog.Nop())
	dst := s3store.NewS3Storer(f.Client(), "us", "repo.git", zerolog.Nop())
	a := commit(t, src, plumbing.ZeroHash, "flag: false")
	require.NoError(t, src.SetReference(plumbing.NewHashReference(branch, a)))
	mirror := Mirror{Source: src, Destination: dst, Branch: branch}
	require.NoError(t, mirror.Reconcile(ctx))
	assertHead := func(want plumbing.Hash) {
		t.Helper()
		r, err := dst.Reference(branch)
		require.NoError(t, err)
		require.Equal(t, want, r.Hash())
		c, err := object.GetCommit(dst, r.Hash())
		require.NoError(t, err)
		_, err = c.Tree()
		require.NoError(t, err)
	}
	assertHead(a)
	b := commit(t, src, a, "flag: true")
	require.NoError(t, src.CheckAndSetReference(plumbing.NewHashReference(branch, b), plumbing.NewHashReference(branch, a)))
	// An interrupted object transfer cannot advance the destination ref.
	f.Before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/us/") && strings.Contains(r.URL.Path, "/objects/") {
			w.WriteHeader(503)
			return true
		}
		return false
	}
	require.Error(t, mirror.Reconcile(ctx))
	assertHead(a)
	f.Before = nil
	// A new controller has no in-memory progress, but resumes from durable refs.
	restarted := Mirror{Source: src, Destination: dst, Branch: branch}
	require.NoError(t, restarted.Reconcile(ctx))
	assertHead(b)
	// A stale source snapshot cannot roll the regional branch back.
	f.Put("eu/repo.git/refs/heads/main", []byte(a.String()), "")
	require.Error(t, restarted.Reconcile(ctx))
	assertHead(b)
	// Lost publication response resolves to a complete committed revision on retry.
	f.Put("eu/repo.git/refs/heads/main", []byte(b.String()), "")
	require.NoError(t, restarted.Reconcile(ctx))
	assertHead(b)
	c, err := object.GetCommit(dst, b)
	require.NoError(t, err)
	file, err := c.File("flags.yaml")
	require.NoError(t, err)
	body, err := file.Contents()
	require.NoError(t, err)
	require.Equal(t, "flag: true", body)
}
func TestReplicationRejectsMissingAndCorruptObjects(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			f := testutil.NewS3(t)
			src := s3store.NewS3Storer(f.Client(), "eu", "repo.git", zerolog.Nop())
			dst := s3store.NewS3Storer(f.Client(), "us", "repo.git", zerolog.Nop())
			h := commit(t, src, plumbing.ZeroHash, "flag: true")
			require.NoError(t, src.SetReference(plumbing.NewHashReference(branch, h)))
			key := "eu/repo.git/objects/" + h.String()[:2] + "/" + h.String()[2:]
			if corrupt {
				f.Put(key, []byte("corrupt"), "commit")
			} else {
				f.Delete(key)
			}
			m := Mirror{Source: src, Destination: dst, Branch: branch}
			require.Error(t, m.Reconcile(context.Background()))
			_, err := dst.Reference(branch)
			require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
		})
	}
}
func TestDuplicateControllersConverge(t *testing.T) {
	f := testutil.NewS3(t)
	src := s3store.NewS3Storer(f.Client(), "eu", "repo.git", zerolog.Nop())
	h := commit(t, src, plumbing.ZeroHash, "flag: true")
	require.NoError(t, src.SetReference(plumbing.NewHashReference(branch, h)))
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := Mirror{Source: src, Destination: s3store.NewS3Storer(f.Client(), "us", "repo.git", zerolog.Nop()), Branch: branch}
			_ = m.Reconcile(context.Background())
		}()
	}
	wg.Wait()
	m := Mirror{Source: src, Destination: s3store.NewS3Storer(f.Client(), "us", "repo.git", zerolog.Nop()), Branch: branch}
	require.NoError(t, m.Reconcile(context.Background()))
	r, err := m.Destination.Reference(branch)
	require.NoError(t, err)
	require.Equal(t, h, r.Hash())
}

func TestMissingReachableBlobBlocksPublication(t *testing.T) {
	f := testutil.NewS3(t)
	src := s3store.NewS3Storer(f.Client(), "eu", "repo.git", zerolog.Nop())
	dst := s3store.NewS3Storer(f.Client(), "us", "repo.git", zerolog.Nop())
	a := commit(t, src, plumbing.ZeroHash, "old")
	require.NoError(t, src.SetReference(plumbing.NewHashReference(branch, a)))
	m := Mirror{Source: src, Destination: dst, Branch: branch}
	require.NoError(t, m.Reconcile(context.Background()))
	b := commit(t, src, a, "new")
	require.NoError(t, src.CheckAndSetReference(plumbing.NewHashReference(branch, b), plumbing.NewHashReference(branch, a)))
	c, err := object.GetCommit(src, b)
	require.NoError(t, err)
	file, err := c.File("flags.yaml")
	require.NoError(t, err)
	hash := file.Hash
	f.Delete("eu/repo.git/objects/" + hash.String()[:2] + "/" + hash.String()[2:])
	require.Error(t, m.Reconcile(context.Background()))
	head, err := dst.Reference(branch)
	require.NoError(t, err)
	require.Equal(t, a, head.Hash())
}
func TestRunRetriesAndCancels(t *testing.T) {
	f := testutil.NewS3(t)
	src := s3store.NewS3Storer(f.Client(), "eu", "repo.git", zerolog.Nop())
	dst := s3store.NewS3Storer(f.Client(), "us", "repo.git", zerolog.Nop())
	m := Mirror{Source: src, Destination: dst, Branch: branch}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status := make(chan Status, 20)
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, 20*time.Millisecond, time.Second, func(s Status) { status <- s }) }()
	select {
	case s := <-status:
		require.NotEmpty(t, s.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("no failed reconciliation status")
	}
	h := commit(t, src, plumbing.ZeroHash, "ready")
	require.NoError(t, src.SetReference(plumbing.NewHashReference(branch, h)))
	require.Eventually(t, func() bool { return m.Status().AppliedRevision == h.String() }, 2*time.Second, 20*time.Millisecond)
	s := m.Status()
	require.Empty(t, s.Error)
	require.False(t, s.LastSuccess.IsZero())
	require.Equal(t, h.String(), s.SourceRevision)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not stop")
	}
}

func TestRegionalReadinessDoesNotDependOnSource(t *testing.T) {
	f := testutil.NewS3(t)
	src := s3store.NewS3Storer(f.Client(), "eu", "repo.git", zerolog.Nop())
	dst := s3store.NewS3Storer(f.Client(), "us", "repo.git", zerolog.Nop())
	m := Mirror{Source: src, Destination: dst, Branch: branch}
	require.Error(t, m.Ready(context.Background()))
	h := commit(t, src, plumbing.ZeroHash, "ready")
	require.NoError(t, src.SetReference(plumbing.NewHashReference(branch, h)))
	require.NoError(t, m.Reconcile(context.Background()))
	f.Before = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/eu/") {
			w.WriteHeader(503)
			return true
		}
		return false
	}
	require.Error(t, m.Reconcile(context.Background()))
	restarted := Mirror{Source: src, Destination: dst, Branch: branch}
	require.NoError(t, restarted.Ready(context.Background()))
}
