package common

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/labbs/git-server-s3/internal/testutil"
	"github.com/labbs/git-server-s3/pkg/storage"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type testRepo struct {
	storage.GitRepositoryStorage
	fixture *testutil.S3
}

func (r testRepo) RepositoryExists(string) bool { return true }
func (r testRepo) GetStorer(string) (storer.Storer, error) {
	return s3store.NewS3Storer(r.fixture.Client(), "b", "repo.git", zerolog.Nop()), nil
}
func makeCommit(t *testing.T, s storer.Storer, parent plumbing.Hash, text string) plumbing.Hash {
	t.Helper()
	tree := object.Tree{}
	obj := s.NewEncodedObject()
	require.NoError(t, tree.Encode(obj))
	th, err := s.SetEncodedObject(obj)
	require.NoError(t, err)
	c := object.Commit{TreeHash: th, Message: text, Author: object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(1, 0)}}
	c.Committer = c.Author
	if !parent.IsZero() {
		c.ParentHashes = []plumbing.Hash{parent}
	}
	obj = s.NewEncodedObject()
	require.NoError(t, c.Encode(obj))
	h, err := s.SetEncodedObject(obj)
	require.NoError(t, err)
	return h
}
func TestReceivePackChecksClientOldHead(t *testing.T) {
	f := testutil.NewS3(t)
	repo := testRepo{fixture: f}
	s, _ := repo.GetStorer("repo.git")
	a := makeCommit(t, s, plumbing.ZeroHash, "a")
	b := makeCommit(t, s, a, "b")
	c := makeCommit(t, s, a, "c")
	ref := plumbing.ReferenceName("refs/heads/main")
	require.NoError(t, s.SetReference(plumbing.NewHashReference(ref, a)))
	telemetry := testutil.ObserveTelemetry(t)
	push := func(old, next plumbing.Hash) error {
		srv, ep, err := GetTransportServer("repo.git", repo)
		if err != nil {
			return err
		}
		sess, err := srv.NewReceivePackSession(ep, nil)
		if err != nil {
			return err
		}
		defer func() { _ = sess.Close() }()
		req := packp.NewReferenceUpdateRequest()
		_ = req.Capabilities.Set(capability.ReportStatus)
		req.Commands = []*packp.Command{{Name: ref, Old: old, New: next}}
		report, err := sess.ReceivePack(context.Background(), req)
		if err == nil {
			return report.Error()
		}
		return err
	}
	// Stale clients must fail even when the winning update finished before they start.
	require.NoError(t, push(a, b))
	require.Error(t, push(a, c))
	current, err := s.Reference(ref)
	require.NoError(t, err)
	require.Equal(t, b, current.Hash())
	// Deletions and incomplete commits may never remove or corrupt the serving branch.
	require.Error(t, push(b, plumbing.ZeroHash))
	require.Error(t, push(b, plumbing.NewHash(strings.Repeat("f", 40))))
	metrics := telemetry.Collect(t)
	require.Contains(t, metrics, "ogit.git.pushes")
	outcomes := map[string]int64{}
	for _, point := range metrics["ogit.git.pushes"].Data.(metricdata.Sum[int64]).DataPoints {
		outcome, _ := point.Attributes.Value("ogit.outcome")
		outcomes[outcome.AsString()] = point.Value
		require.Equal(t, 1, point.Attributes.Len())
	}
	require.Equal(t, map[string]int64{"success": 1, "conflict": 1, "rejected": 1, "error": 1}, outcomes)
	require.Contains(t, metrics, "ogit.ref.publications")
	publications := map[string]int64{}
	for _, point := range metrics["ogit.ref.publications"].Data.(metricdata.Sum[int64]).DataPoints {
		outcome, _ := point.Attributes.Value("ogit.outcome")
		publications[outcome.AsString()] += point.Value
	}
	require.Equal(t, map[string]int64{"success": 1, "conflict": 1}, publications)

}
func TestConcurrentReceivePackHasOneWinner(t *testing.T) {
	f := testutil.NewS3(t)
	repo := testRepo{fixture: f}
	s, _ := repo.GetStorer("repo.git")
	a := makeCommit(t, s, plumbing.ZeroHash, "a")
	heads := []plumbing.Hash{makeCommit(t, s, a, "b"), makeCommit(t, s, a, "c")}
	ref := plumbing.ReferenceName("refs/heads/main")
	require.NoError(t, s.SetReference(plumbing.NewHashReference(ref, a)))
	var wg sync.WaitGroup
	results := make(chan plumbing.Hash, 2)
	start := make(chan struct{})
	for _, h := range heads {
		wg.Add(1)
		go func(h plumbing.Hash) {
			defer wg.Done()
			srv, ep, _ := GetTransportServer("repo.git", repo)
			sess, _ := srv.NewReceivePackSession(ep, nil)
			defer func() { _ = sess.Close() }()
			req := packp.NewReferenceUpdateRequest()
			_ = req.Capabilities.Set(capability.ReportStatus)
			req.Commands = []*packp.Command{{Name: ref, Old: a, New: h}}
			<-start
			report, err := sess.ReceivePack(context.Background(), req)
			if err == nil && report.Error() == nil {
				results <- h
			}
		}(h)
	}
	close(start)
	wg.Wait()
	close(results)
	var winners []plumbing.Hash
	for h := range results {
		winners = append(winners, h)
	}
	require.Len(t, winners, 1)
	r, err := s.Reference(ref)
	require.NoError(t, err)
	require.Equal(t, winners[0], r.Hash())
}
